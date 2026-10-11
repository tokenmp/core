package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/tokenmp/core/channel"
	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/dialect"
	"github.com/tokenmp/core/plugin"
	"github.com/tokenmp/core/relay"
)

// writeStream 处理流式响应。
func (s *Server) writeStream(
	writer http.ResponseWriter,
	request *http.Request,
	client dialect.Name,
	model string,
	route config.Route,
	prepared *relay.Prepared,
	response *http.Response,
) {
	ctx := request.Context()
	// 会话先建好再写响应头：建不出来时还能正常回一个错误状态码，
	// 而不是先写出 200 再留一条空的流。
	var session *relay.Stream
	if !prepared.Passthrough {
		created, err := s.engine.NewStream(route.Dialect, client, model, route.Upstream, relay.StreamOptions{
			Model:        model,
			Created:      s.now().Unix(),
			IncludeUsage: true,
		})
		if err != nil {
			s.logger.Warn("创建流式转换会话失败", "request_id", requestIDOf(ctx), "error", err.Error())
			s.writeError(ctx, writer, http.StatusBadGateway, "conversion_error", err.Error())
			return
		}
		session = created
	}
	header := writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	out := channel.NewWriter(writer)

	if session == nil {
		s.pipeStream(ctx, writer, client, model, response, out, flusher)
		return
	}
	s.convertStream(ctx, client, model, session, response, out)
}

// pipeStream 转发同方言的上游流。没有插件订阅 onEvent 时按字节搬运，原始报文
// 逐字节保留。
func (s *Server) pipeStream(
	ctx context.Context,
	writer http.ResponseWriter,
	client dialect.Name,
	model string,
	response *http.Response,
	out *channel.Writer,
	flusher http.Flusher,
) {
	if !s.plugins.HasAny(plugin.HookEvent) {
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		// 搬运继续交给 channel.CopyRaw，本路径不动它一个字节。用量在上游一侧
		// 旁路扫描：事件读取器会归一化换行、丢掉注释行与未知字段，用它回写就
		// 不是逐字节透传了，因此扫描必须与写出分开。
		source := &usageTeeReader{source: response.Body, client: client}
		if _, err := channel.CopyRaw(writer, source, flush); err != nil {
			s.logger.Warn("转发上游流失败", "request_id", requestIDOf(ctx), "error", err.Error())
			return
		}
		// 上游没给用量时保持原样，不记 nil。
		if source.usage != nil {
			accessOf(ctx).recordUsage(source.usage)
		}
		return
	}
	reader := channel.NewReader(response.Body)
	var usage *dto.Usage
	for {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.logger.Warn("读取上游流失败", "request_id", requestIDOf(ctx), "error", err.Error())
			return
		}
		if dialect.IsDoneMarker(event.Data) {
			if err := out.WriteRaw(event.Data); err != nil {
				s.logger.Warn("写出结束标记失败", "request_id", requestIDOf(ctx), "error", err.Error())
				return
			}
			continue
		}
		// 这条路径本来就在逐条解析，用量顺手取自插件改写之前的原分片。
		if found := streamUsageOf(client, event.Data); found != nil {
			usage = found
		}
		if err := s.emitEvent(ctx, client, model, event.Name, event.Data, out); err != nil {
			s.logger.Warn("写出流式事件失败", "request_id", requestIDOf(ctx), "error", err.Error())
			return
		}
	}
	if usage != nil {
		accessOf(ctx).recordUsage(usage)
	}
}

// streamUsageOf 从一条流式分片里取用量。
//
// 只认 OpenAI 系两个方言：Claude 与 Gemini 的用量不以这个形状出现在透传分片里。
// 透传路径不保证分片是 JSON，解析失败返回 nil，不能因为它让整个请求失败。
func streamUsageOf(client dialect.Name, data []byte) *dto.Usage {
	if client != dialect.OpenAIChat && client != dialect.OpenAIResponses {
		return nil
	}
	// 廉价预筛：先找一个精确的字节子串，命中才做完整解析。JSON 字符串里被转义的
	// 引号写作 \"，所以正文里提到 usage 这个词（写作 \"usage\"）不会命中，
	// 只有真正的 "usage" 键才会。
	if !bytes.Contains(data, []byte(`"usage"`)) {
		return nil
	}
	decoded, err := dialect.DecodeStreamResponse(client, data)
	if err != nil {
		return nil
	}
	switch response := decoded.(type) {
	case *dto.ChatCompletionsStreamResponse:
		return response.Usage
	case *dto.ResponsesStreamResponse:
		if response.Response == nil {
			return nil
		}
		return response.Response.Usage
	default:
		return nil
	}
}

// usageTeeReader 把上游流原样交给 channel.CopyRaw，同时旁路扫描 SSE 分片里的用量。
//
// Read 返回的字节与读到的完全一致，不改一个字节；扫描只在自己的副本上按 SSE 的
// 常用子集切出 data 负载，命中 "usage" 时解析一次。usage 只由本次搬运所在的
// goroutine 写，搬运结束后 pipeStream 读，不需要锁。
type usageTeeReader struct {
	source io.Reader
	client dialect.Name
	// usage 是扫描到的最新一份用量，上游没给时为 nil。
	usage *dto.Usage
	// partial 是尚未见到换行的当前行，data 是当前事件拼接中的 data 负载。
	partial []byte
	data    []byte
	hasData bool
	// overflow 标记当前事件已经超过扫描上限，后续 data 行不再拼接。
	overflow bool
}

// Read 读一段原样返回，并把它交给扫描。
func (r *usageTeeReader) Read(p []byte) (int, error) {
	read, err := r.source.Read(p)
	if read > 0 {
		r.scan(p[:read])
	}
	if err != nil {
		// 最后一行可能没有换行符，最后一条事件可能没有空行收尾。
		r.finish()
	}
	return read, err
}

// scan 把这一段按换行切成整行，余下的留在 partial 里等下一段。
func (r *usageTeeReader) scan(chunk []byte) {
	for len(chunk) > 0 {
		index := bytes.IndexByte(chunk, '\n')
		if index < 0 {
			r.partial = append(r.partial, chunk...)
			return
		}
		r.partial = append(r.partial, chunk[:index]...)
		r.line(r.partial)
		r.partial = r.partial[:0]
		chunk = chunk[index+1:]
	}
}

// line 处理一整行。切分规则与 channel.Reader 对齐：行尾 CR 去掉，冒号开头是
// 注释，data 行按行拼接，空行分发事件。
func (r *usageTeeReader) line(raw []byte) {
	line := bytes.TrimRight(raw, "\r")
	if len(line) == 0 {
		r.flush()
		return
	}
	if line[0] == ':' {
		return
	}
	field, value, found := bytes.Cut(line, []byte(":"))
	if !found {
		field, value = line, nil
	}
	if string(field) != "data" {
		return
	}
	value = bytes.TrimPrefix(value, []byte(" "))
	if r.overflow {
		return
	}
	// 上限与 channel.Reader 一致：真正的用量分片只有几百字节，这里只是防止
	// 超长分片（例如内联图片）把整条事件读进内存，反而比原来更吃内存。
	if len(r.data)+len(value) > channel.DefaultMaxEventBytes {
		r.overflow = true
		r.data = r.data[:0]
		r.hasData = false
		return
	}
	if r.hasData {
		r.data = append(r.data, '\n')
	}
	r.data = append(r.data, value...)
	r.hasData = true
}

// flush 收尾一条完整事件，取其中的用量。
func (r *usageTeeReader) flush() {
	if r.hasData {
		if usage := streamUsageOf(r.client, r.data); usage != nil {
			r.usage = usage
		}
	}
	r.data = r.data[:0]
	r.hasData = false
	r.overflow = false
}

// finish 在流结束时补收尾：把残留的末行与末事件都交给扫描。
func (r *usageTeeReader) finish() {
	if len(r.partial) > 0 {
		r.line(r.partial)
		r.partial = r.partial[:0]
	}
	r.flush()
}

// convertStream 转发跨方言的上游流。
//
// 上游中途出错时不补终止帧：客户端收不到结束标记就知道这一条回答被截断了。
// 补一个正常的终止帧会让截断看起来像正常结束。
func (s *Server) convertStream(
	ctx context.Context,
	client dialect.Name,
	model string,
	stream *relay.Stream,
	response *http.Response,
	out *channel.Writer,
) {
	reader := channel.NewReader(response.Body)
	failure := ""
	for {
		event, readErr := reader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			failure = readErr.Error()
			break
		}
		if dialect.IsDoneMarker(event.Data) {
			continue
		}
		converted, convertErr := stream.Convert(ctx, event.Data)
		if convertErr != nil {
			failure = convertErr.Error()
			break
		}
		if writeErr := s.writeEvents(ctx, client, model, converted, out); writeErr != nil {
			s.logger.Warn("写出流式事件失败", "request_id", requestIDOf(ctx), "error", writeErr.Error())
			return
		}
	}
	if failure != "" {
		s.logger.Warn("上游流中断", "request_id", requestIDOf(ctx), "model", model, "error", failure)
		if events, ok := stream.Fail("upstream_stream_error", failure, ""); ok {
			if writeErr := s.writeEvents(ctx, client, model, events, out); writeErr != nil {
				s.logger.Warn("写出错误事件失败", "request_id", requestIDOf(ctx), "error", writeErr.Error())
			}
		}
		return
	}
	final, err := stream.Finalize(ctx)
	if err != nil {
		s.logger.Warn("流式收尾失败", "request_id", requestIDOf(ctx), "error", err.Error())
	}
	usage := stream.Usage()
	final = s.rewriteStreamEnd(ctx, client, model, usage, final)
	if err := s.writeEvents(ctx, client, model, final, out); err != nil {
		s.logger.Warn("写出收尾事件失败", "request_id", requestIDOf(ctx), "error", err.Error())
		return
	}
	if client == dialect.OpenAIChat {
		if err := out.WriteRaw(dialect.DoneEvent()); err != nil {
			s.logger.Warn("写出结束标记失败", "request_id", requestIDOf(ctx), "error", err.Error())
		}
	}
	// 用量交给访问日志那一行，这里不再单独记一条。
	accessOf(ctx).recordUsage(usage)
}

// writeEvents 逐条写出事件，每条都过一遍 onEvent。
func (s *Server) writeEvents(ctx context.Context, client dialect.Name, model string, events []dialect.StreamEvent, out *channel.Writer) error {
	for _, event := range events {
		if err := s.emitEvent(ctx, client, model, event.Event, event.Data, out); err != nil {
			return err
		}
	}
	return nil
}

// emitEvent 写出一条事件。启用插件导出了 onEvent 时先让它改写。
func (s *Server) emitEvent(ctx context.Context, client dialect.Name, model, event string, data []byte, out *channel.Writer) error {
	if len(data) == 0 {
		return nil
	}
	name, payload := event, data
	if s.plugins.HasAny(plugin.HookEvent) {
		rewritten, err := s.rewriteEvent(ctx, client, name, payload)
		if err != nil {
			s.logger.Warn("事件钩子执行失败", "request_id", requestIDOf(ctx), "error", err.Error())
		} else {
			name, payload = rewritten.Name, rewritten.Data
		}
	}
	if len(payload) == 0 {
		return nil
	}
	return out.Write(channel.Event{Name: name, Data: payload})
}

// rewriteEvent 调用插件的事件钩子。
func (s *Server) rewriteEvent(ctx context.Context, client dialect.Name, event string, data []byte) (channel.Event, error) {
	payload, err := json.Marshal(eventPayload{
		Dialect: string(client),
		Event:   event,
		Data:    json.RawMessage(data),
	})
	if err != nil {
		return channel.Event{}, err
	}
	outcome := s.plugins.Apply(ctx, plugin.HookEvent, payload, mergeEvent, nil, s.session())
	if !outcome.Changed() {
		return channel.Event{Name: event, Data: data}, nil
	}
	var final eventPayload
	if err := json.Unmarshal(outcome.Data, &final); err != nil {
		return channel.Event{}, err
	}
	updated := channel.Event{Name: event, Data: data}
	if final.Event != "" {
		updated.Name = final.Event
	}
	if len(final.Data) > 0 {
		updated.Data = final.Data
	}
	return updated, nil
}

// rewriteStreamEnd 让插件改写信封收尾阶段补发的分片。
func (s *Server) rewriteStreamEnd(ctx context.Context, client dialect.Name, model string, usage *dto.Usage, events []dialect.StreamEvent) []dialect.StreamEvent {
	if !s.plugins.HasAny(plugin.HookStreamEnd) || len(events) == 0 {
		return events
	}
	items := make([]streamEvent, 0, len(events))
	for _, event := range events {
		items = append(items, streamEvent{Event: event.Event, Data: event.Data})
	}
	usageJSON, err := json.Marshal(usage)
	if err != nil {
		usageJSON = json.RawMessage("null")
	}
	payload, err := json.Marshal(streamEndPayload{
		Dialect: string(client),
		Model:   model,
		Usage:   usageJSON,
		Events:  items,
	})
	if err != nil {
		s.logger.Warn("构造收尾钩子负载失败", "request_id", requestIDOf(ctx), "error", err.Error())
		return events
	}
	outcome := s.plugins.Apply(ctx, plugin.HookStreamEnd, payload, mergeStreamEnd, nil, s.session())
	if !outcome.Changed() {
		return events
	}
	var final streamEndPayload
	if err := json.Unmarshal(outcome.Data, &final); err != nil {
		s.logger.Warn("解析收尾钩子输出失败", "request_id", requestIDOf(ctx), "error", err.Error())
		return events
	}
	if final.Events == nil {
		return events
	}
	updated := make([]dialect.StreamEvent, 0, len(final.Events))
	for _, item := range final.Events {
		updated = append(updated, dialect.StreamEvent{Event: item.Event, Data: item.Data})
	}
	return updated
}
