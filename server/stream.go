package server

import (
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
	header := writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	out := channel.NewWriter(writer)

	if prepared.Passthrough {
		s.pipeStream(ctx, writer, client, model, response, out, flusher)
		return
	}
	s.convertStream(ctx, client, model, route, response, out)
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
		if _, err := channel.CopyRaw(writer, response.Body, flush); err != nil {
			s.logger.Warn("转发上游流失败", "error", err.Error())
		}
		return
	}
	reader := channel.NewReader(response.Body)
	for {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			s.logger.Warn("读取上游流失败", "error", err.Error())
			return
		}
		if dialect.IsDoneMarker(event.Data) {
			if err := out.WriteRaw(event.Data); err != nil {
				s.logger.Warn("写出结束标记失败", "error", err.Error())
				return
			}
			continue
		}
		if err := s.emitEvent(ctx, client, model, event.Name, event.Data, out); err != nil {
			s.logger.Warn("写出流式事件失败", "error", err.Error())
			return
		}
	}
}

// convertStream 转发跨方言的上游流。
func (s *Server) convertStream(
	ctx context.Context,
	client dialect.Name,
	model string,
	route config.Route,
	response *http.Response,
	out *channel.Writer,
) {
	stream, err := s.engine.NewStream(route.Dialect, client, model, route.Upstream, relay.StreamOptions{
		Model:        model,
		Created:      s.now().Unix(),
		IncludeUsage: true,
	})
	if err != nil {
		s.logger.Warn("创建流式转换会话失败", "error", err.Error())
		return
	}
	reader := channel.NewReader(response.Body)
	for {
		event, readErr := reader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			s.logger.Warn("读取上游流失败", "error", readErr.Error())
			break
		}
		if dialect.IsDoneMarker(event.Data) {
			continue
		}
		converted, convertErr := stream.Convert(ctx, event.Data)
		if convertErr != nil {
			s.logger.Warn("转换流式分片失败", "error", convertErr.Error())
			break
		}
		if writeErr := s.writeEvents(ctx, client, model, converted, out); writeErr != nil {
			s.logger.Warn("写出流式事件失败", "error", writeErr.Error())
			return
		}
	}
	final, err := stream.Finalize(ctx)
	if err != nil {
		s.logger.Warn("流式收尾失败", "error", err.Error())
	}
	final = s.rewriteStreamEnd(ctx, client, model, stream.Usage(), final)
	if err := s.writeEvents(ctx, client, model, final, out); err != nil {
		s.logger.Warn("写出收尾事件失败", "error", err.Error())
		return
	}
	if client == dialect.OpenAIChat {
		if err := out.WriteRaw(dialect.DoneEvent()); err != nil {
			s.logger.Warn("写出结束标记失败", "error", err.Error())
		}
	}
	if usage := stream.Usage(); usage != nil {
		s.logger.Debug("流式请求完成", "model", model, "usage", usage)
	}
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
			s.logger.Warn("事件钩子执行失败", "error", err.Error())
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
	outcome := s.plugins.Invoke(ctx, plugin.HookEvent, payload, nil, s.session())
	if !outcome.Changed() {
		return channel.Event{Name: event, Data: data}, nil
	}
	var rewrite eventRewrite
	if err := json.Unmarshal(outcome.Data, &rewrite); err != nil {
		return channel.Event{}, err
	}
	updated := channel.Event{Name: event, Data: data}
	if rewrite.Event != nil {
		updated.Name = *rewrite.Event
	}
	if len(rewrite.Data) > 0 {
		updated.Data = rewrite.Data
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
		s.logger.Warn("构造收尾钩子负载失败", "error", err.Error())
		return events
	}
	outcome := s.plugins.Invoke(ctx, plugin.HookStreamEnd, payload, nil, s.session())
	if !outcome.Changed() {
		return events
	}
	var rewrite streamEndRewrite
	if err := json.Unmarshal(outcome.Data, &rewrite); err != nil {
		s.logger.Warn("解析收尾钩子输出失败", "error", err.Error())
		return events
	}
	if rewrite.Events == nil {
		return events
	}
	updated := make([]dialect.StreamEvent, 0, len(rewrite.Events))
	for _, item := range rewrite.Events {
		updated = append(updated, dialect.StreamEvent{Event: item.Event, Data: item.Data})
	}
	return updated
}
