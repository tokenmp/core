package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tokenmp/core/channel"
	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/dialect"
	"github.com/tokenmp/core/plugin"
	"github.com/tokenmp/core/relay"
)

// maxErrorBodyBytes 是记入日志的上游错误响应上限。
const maxErrorBodyBytes = 8 << 10

// forward 是转发主流程：改写请求、选路、调用上游、写回响应。
func (s *Server) forward(writer http.ResponseWriter, request *http.Request, client dialect.Name, pathModel string, streamHint bool) {
	ctx := request.Context()
	body, ok := s.readBody(writer, request)
	if !ok {
		return
	}

	if _, err := dialect.DecodeRequest(client, body); err != nil {
		s.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	stream, err := dialect.StreamIntent(client, body, streamHint)
	if err != nil {
		s.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	model := pathModel
	if model == "" {
		model, err = dialect.ModelOf(client, body)
		if err != nil {
			s.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}

	body, model = s.rewriteRequest(ctx, client, model, stream, body)
	routes := s.config.Resolve(model)
	if len(routes) == 0 {
		s.writeError(writer, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("没有匹配模型 %s 的路由", model))
		return
	}

	response, prepared, route := s.callUpstream(ctx, writer, client, model, stream, body, routes)
	if response == nil {
		return
	}
	defer func() { _ = response.Body.Close() }()
	s.writeUpstream(writer, request, client, model, route, prepared, response)
}

// callUpstream 按候选路由依次尝试，返回第一个可用的上游响应。
//
// 有剩余候选且失败属于「渠道问题」时继续试下一条：连接错误、408、429、401、
// 403 与 5xx。400、404、422 这类说明请求本身有问题，换渠道不会变好，直接返回。
// 全部候选都失败时写出最后一次的状态码与前一次尝试的失败原因。
func (s *Server) callUpstream(
	ctx context.Context,
	writer http.ResponseWriter,
	client dialect.Name,
	model string,
	stream bool,
	body []byte,
	routes []config.Route,
) (*http.Response, *relay.Prepared, config.Route) {
	var lastMessage string
	for index, route := range routes {
		upstream, ok := s.channels.Get(route.Channel)
		if !ok {
			lastMessage = fmt.Sprintf("渠道 %s 不存在", route.Channel)
			continue
		}
		prepared, err := s.engine.PrepareRequest(ctx, relay.Request{
			From:          client,
			To:            route.Dialect,
			Body:          body,
			OriginModel:   model,
			UpstreamModel: route.Upstream,
			Stream:        stream,
		})
		if err != nil {
			lastMessage = err.Error()
			s.logger.Warn("请求转换失败", "route", routeOf(route), "error", err.Error())
			continue
		}
		response, err := upstream.Send(ctx, channel.Request{
			Model:  route.Upstream,
			Stream: prepared.Stream,
			Body:   prepared.Body,
		})
		if err != nil {
			lastMessage = err.Error()
			s.logger.Warn("上游请求失败", "route", routeOf(route), "error", err.Error())
			continue
		}
		if response.StatusCode >= http.StatusBadRequest {
			if shouldTryNext(response.StatusCode, index, len(routes)) {
				lastMessage = readErrorBody(response)
				s.logger.Warn("上游拒绝请求，改试下一条路由", "route", routeOf(route), "status", response.StatusCode)
				continue
			}
			// 不可重试的状态，或已经是最后一条候选：原样交给调用方。
			return response, prepared, route
		}
		if len(prepared.Diagnostics()) > 0 {
			s.logger.Debug("转换损耗",
				"route", routeOf(route),
				"count", len(prepared.Diagnostics()),
				"diagnostics", prepared.Diagnostics())
		}
		return response, prepared, route
	}
	if lastMessage == "" {
		lastMessage = "没有可用的候选路由"
	}
	s.writeError(writer, http.StatusBadGateway, "upstream_error", lastMessage)
	return nil, nil, config.Route{}
}

// writeUpstream 把上游响应写回客户端。
func (s *Server) writeUpstream(
	writer http.ResponseWriter,
	request *http.Request,
	client dialect.Name,
	model string,
	route config.Route,
	prepared *relay.Prepared,
	response *http.Response,
) {
	copyResponseHeaders(writer, response, prepared.Passthrough)
	if response.StatusCode >= http.StatusBadRequest {
		// 错误响应按原样透传：不同上游的错误形状差异很大，翻译只会丢掉排查线索。
		writer.WriteHeader(response.StatusCode)
		if _, err := io.Copy(writer, io.LimitReader(response.Body, s.maxBodyBytes)); err != nil {
			s.logger.Warn("写出上游错误响应失败", "error", err.Error())
		}
		return
	}
	if prepared.Stream && isEventStream(response) {
		s.writeStream(writer, request, client, model, route, prepared, response)
		return
	}
	s.writeBuffered(writer, request, client, model, route, prepared, response)
}

// writeBuffered 处理非流式响应。
func (s *Server) writeBuffered(
	writer http.ResponseWriter,
	request *http.Request,
	client dialect.Name,
	model string,
	route config.Route,
	prepared *relay.Prepared,
	response *http.Response,
) {
	body, err := io.ReadAll(io.LimitReader(response.Body, s.maxBodyBytes))
	if err != nil {
		s.writeError(writer, http.StatusBadGateway, "upstream_error", fmt.Sprintf("读取上游响应失败: %v", err))
		return
	}
	converted, err := s.engine.PrepareResponse(request.Context(), relay.Response{
		From:          route.Dialect,
		To:            client,
		Body:          body,
		OriginModel:   model,
		UpstreamModel: route.Upstream,
	})
	if err != nil {
		s.logger.Warn("响应转换失败", "error", err.Error())
		s.writeError(writer, http.StatusBadGateway, "conversion_error", err.Error())
		return
	}
	out := converted.Body
	out = s.rewriteResponse(request.Context(), client, model, out)
	if !converted.Passthrough {
		writer.Header().Set("Content-Type", "application/json")
	}
	writer.WriteHeader(http.StatusOK)
	if _, err := writer.Write(out); err != nil {
		s.logger.Warn("写出响应失败", "error", err.Error())
	}
	if converted.Usage != nil {
		s.logger.Debug("请求完成", "model", model, "usage", converted.Usage)
	}
}

// rewriteRequest 依次调用启用插件的请求钩子。没有插件参与时不动原始字节。
func (s *Server) rewriteRequest(ctx context.Context, client dialect.Name, model string, stream bool, body []byte) ([]byte, string) {
	if !s.plugins.HasAny(plugin.HookRequest) {
		return body, model
	}
	payload, err := json.Marshal(requestPayload{
		Dialect: string(client),
		Model:   model,
		Stream:  stream,
		Body:    json.RawMessage(body),
	})
	if err != nil {
		s.logger.Warn("构造请求钩子负载失败", "error", err.Error())
		return body, model
	}
	outcome := s.plugins.Invoke(ctx, plugin.HookRequest, payload, nil, s.session())
	if !outcome.Changed() {
		return body, model
	}
	var rewrite requestRewrite
	if err := json.Unmarshal(outcome.Data, &rewrite); err != nil {
		s.logger.Warn("解析请求钩子输出失败", "plugins", outcome.Applied, "error", err.Error())
		return body, model
	}
	updated := body
	if len(rewrite.Body) > 0 {
		updated = rewrite.Body
	}
	if rewrite.Model != nil && *rewrite.Model != "" {
		model = *rewrite.Model
	}
	s.logger.Debug("请求钩子生效", "plugins", outcome.Applied, "model", model)
	return updated, model
}

// rewriteResponse 依次调用启用插件的响应钩子。
func (s *Server) rewriteResponse(ctx context.Context, client dialect.Name, model string, body []byte) []byte {
	if !s.plugins.HasAny(plugin.HookResponse) {
		return body
	}
	payload, err := json.Marshal(responsePayload{
		Dialect: string(client),
		Model:   model,
		Body:    json.RawMessage(body),
	})
	if err != nil {
		s.logger.Warn("构造响应钩子负载失败", "error", err.Error())
		return body
	}
	outcome := s.plugins.Invoke(ctx, plugin.HookResponse, payload, nil, s.session())
	if !outcome.Changed() {
		return body
	}
	var rewrite responseRewrite
	if err := json.Unmarshal(outcome.Data, &rewrite); err != nil {
		s.logger.Warn("解析响应钩子输出失败", "plugins", outcome.Applied, "error", err.Error())
		return body
	}
	if len(rewrite.Body) == 0 {
		return body
	}
	s.logger.Debug("响应钩子生效", "plugins", outcome.Applied)
	return rewrite.Body
}

// session 返回本次调用的宿主环境。
func (s *Server) session() plugin.Session {
	return plugin.Session{Now: s.now(), Logger: s.logger}
}

// shouldTryNext 报告是否值得换下一条候选路由。
func shouldTryNext(status, index, total int) bool {
	if index >= total-1 {
		return false
	}
	switch {
	case status == http.StatusRequestTimeout,
		status == http.StatusUnauthorized,
		status == http.StatusForbidden,
		status == http.StatusTooManyRequests,
		status >= http.StatusInternalServerError:
		return true
	default:
		return false
	}
}

// isEventStream 报告上游响应是否是 SSE。
func isEventStream(response *http.Response) bool {
	return strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream")
}

// copyResponseHeaders 复制需要透传的响应头。
func copyResponseHeaders(writer http.ResponseWriter, response *http.Response, passthrough bool) {
	for _, name := range []string{"Content-Type", "Retry-After", "X-Request-Id"} {
		value := response.Header.Get(name)
		if value == "" {
			continue
		}
		if name == "Content-Type" && !passthrough {
			// 转换过的响应由本方重新生成 body，Content-Type 固定为 JSON 或 SSE。
			continue
		}
		writer.Header().Set(name, value)
	}
}

// readErrorBody 读出一段上游错误响应并关闭它，用于日志与最终错误信息。
func readErrorBody(response *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))
	_ = response.Body.Close()
	if err != nil {
		return fmt.Sprintf("读取上游错误响应失败: %v", err)
	}
	if len(body) == 0 {
		return fmt.Sprintf("上游返回 %s", response.Status)
	}
	return fmt.Sprintf("上游返回 %s: %s", response.Status, string(body))
}
