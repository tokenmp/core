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

// endpoint 描述这个入口在转发流程里的形状。零值是方言自己的主端点：不追加路径
// 后缀，也不限定候选方言。
//
// 同一个方言上有多条端点时靠它区分：计数端点与 Messages 共用一套请求体形状，
// 但发往上游的路径不同，服务它的候选方言也不同。
type endpoint struct {
	// suffix 追加在解析出的上游路径之后，用于同一个方言下的子端点。为空表示不追加。
	suffix string
	// dialectOnly 限定只有这个方言的候选能服务这个入口。空串表示不限定。
	dialectOnly dialect.Name
}

// forward 是转发主流程：改写请求、选路、调用上游、写回响应。
func (s *Server) forward(
	writer http.ResponseWriter,
	request *http.Request,
	client dialect.Name,
	pathModel string,
	streamHint bool,
	shape endpoint,
) {
	ctx := request.Context()
	body, ok := s.readBody(writer, request)
	if !ok {
		return
	}

	if _, err := dialect.DecodeRequest(client, body); err != nil {
		s.writeError(ctx, writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	stream, err := dialect.StreamIntent(client, body, streamHint)
	if err != nil {
		s.writeError(ctx, writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	model := pathModel
	if model == "" {
		model, err = dialect.ModelOf(client, body)
		if err != nil {
			s.writeError(ctx, writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}

	body, model, pluginHeaders := s.rewriteRequest(ctx, client, model, stream, body)
	// 插件可能改过模型名，记录以改写后的为准。
	accessOf(ctx).setModel(model)
	accessOf(ctx).setStream(stream)
	routes, rejected := usableRoutes(s.config.Resolve(model))
	if len(routes) == 0 {
		if rejected != "" {
			s.writeError(ctx, writer, http.StatusBadRequest, "invalid_model", rejected)
			return
		}
		s.writeError(ctx, writer, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("没有匹配模型 %s 的路由", model))
		return
	}
	if shape.dialectOnly != "" {
		// 这个入口只有一种方言的候选服务得了。模型的候选里一条都没有时回 501
		// 而不是 404：模型是配了的，缺的是这个入口要的那种渠道，两者的排查方向
		// 不同。客户端对计数端点的 501 有自己的降级路径，因此这里不会让它退出。
		if routes = onlyDialect(routes, shape.dialectOnly); len(routes) == 0 {
			s.logger.Warn("模型没有该入口可用的候选渠道",
				"request_id", requestIDOf(ctx),
				"model", model,
				"dialect", shape.dialectOnly)
			s.writeError(ctx, writer, http.StatusNotImplemented, "not_supported",
				fmt.Sprintf("模型 %s 的候选里没有 %s 方言的渠道，该端点由上游计算，本网关不本地估算",
					model, shape.dialectOnly))
			return
		}
	}
	// 亲和只在方言自己的主端点上参与：计数这类子端点不写上游缓存，没有可粘的理由，
	// 因此既不按会话重排候选，也不往前缀链里记一笔。
	plan := affinityPlan{routes: routes}
	if shape.main() {
		plan = s.planAffinity(request, client, body, routes)
	}

	response, prepared, route := s.callUpstream(ctx, writer, request, client, model, stream, body, plan.routes, shape, pluginHeaders)
	if response == nil {
		return
	}
	defer func() { _ = response.Body.Close() }()
	if len(plan.chain) > 0 && response.StatusCode < http.StatusBadRequest {
		// 记下真实服务的那条渠道，而不是选定的那条：降级换过渠道之后，下一轮
		// 应当沿用的是缓存实际写在哪。
		//
		// 失败响应不记。上游返回 400、404、422 说明请求本身有问题，它没有写下
		// 任何缓存，记下来只会让这段会话之后每一轮都先撞同一条坏渠道。
		s.affinityTable.Remember(plan.chain, route.Channel)
	}
	s.writeUpstream(writer, request, client, model, route, prepared, response)
}

// main 报告这个入口是不是方言自己的主端点。主端点之外的都是子端点。
func (e endpoint) main() bool { return e.suffix == "" }

// onlyDialect 只留下给定方言的候选。一条都没有时返回空切片。
func onlyDialect(routes []config.Route, name dialect.Name) []config.Route {
	kept := make([]config.Route, 0, len(routes))
	for _, route := range routes {
		if route.Dialect == name {
			kept = append(kept, route)
		}
	}
	return kept
}

// usableRoutes 过滤掉模型名不适合写进上游路径的路由。
//
// 返回的第二个值非空表示所有候选都因此被过滤掉，里面是第一条原因。
// 模型名来自客户端，不能直接拼进 URL，因此这里在发出请求之前就拦下。
func usableRoutes(routes []config.Route) ([]config.Route, string) {
	usable := make([]config.Route, 0, len(routes))
	reason := ""
	for _, route := range routes {
		if route.Dialect == dialect.GeminiGenerate {
			if err := dialect.ValidateModelName(route.Upstream); err != nil {
				if reason == "" {
					reason = err.Error()
				}
				continue
			}
		}
		usable = append(usable, route)
	}
	if len(usable) == 0 && len(routes) > 0 && reason == "" {
		return nil, "没有可用的候选路由"
	}
	return usable, reason
}

// callUpstream 按候选路由依次尝试，返回第一个可用的上游响应。
//
// 有剩余候选且失败属于「渠道问题」时继续试下一条。判据读候选渠道的 spec：
// 默认是 408、429、401、403 与 5xx，配置可以按渠道覆盖。400、404、422 这类
// 说明请求本身有问题，换渠道不会变好，默认直接返回。
// 所有候选都在建立连接阶段失败时写出 502 与最后一条失败原因。
func (s *Server) callUpstream(
	ctx context.Context,
	writer http.ResponseWriter,
	request *http.Request,
	client dialect.Name,
	model string,
	stream bool,
	body []byte,
	routes []config.Route,
	shape endpoint,
	pluginHeaders map[string]string,
) (*http.Response, *relay.Prepared, config.Route) {
	// 宿主按请求设的上游头只取一次，候选换渠道时不变。
	hostHeaders := s.upstreamHeaders(ctx)
	var lastMessage string
	for index, route := range routes {
		started := s.now()
		upstream, ok := s.channels.Get(route.Channel)
		if !ok {
			lastMessage = fmt.Sprintf("渠道 %s 不存在", route.Channel)
			accessOf(ctx).recordAttempt(route, 0, FailureChannel, 0, s.now().Sub(started))
			continue
		}
		headers := s.forwardHeaders(request, route.Channel, upstream.Spec().Headers, pluginHeaders, hostHeaders)
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
			s.logger.Warn("请求转换失败", "request_id", requestIDOf(ctx), "route", routeOf(route), "error", err.Error())
			accessOf(ctx).recordAttempt(route, 0, FailureConvert, 0, s.now().Sub(started))
			continue
		}
		// retries 由渠道内重试的回调累计。回调同步跑在这段代码所在的 goroutine 上，
		// 因此 Send 返回时读它是安全的，不需要锁。
		retries := 0
		response, err := upstream.Send(ctx, channel.Request{
			Dialect:    route.Dialect,
			Model:      route.Upstream,
			Stream:     prepared.Stream,
			Body:       prepared.Body,
			Header:     headers,
			PathSuffix: shape.suffix,
			OnRetry: func(info channel.RetryInfo) {
				retries = info.Attempt
				s.logger.Debug("渠道内重试",
					"request_id", requestIDOf(ctx),
					"route", routeOf(route),
					"retry", info.Attempt,
					"status", info.Status,
					"wait", info.Wait)
			},
		})
		if err != nil {
			lastMessage = err.Error()
			s.logger.Warn("上游请求失败", "request_id", requestIDOf(ctx), "route", routeOf(route), "error", err.Error())
			accessOf(ctx).recordAttempt(route, 0, FailureConnect, retries, s.now().Sub(started))
			continue
		}
		// Send 返回一个可重试的状态码但重试还没用尽，说明上游给的 Retry-After
		// 超过上限，Send 主动放弃了这次重试。记一行 WARN，方便在访问日志里对上
		// 「为什么只发了一次」。max_retries 为 0 时不记。
		// retries 用尽时另记一行 DEBUG。
		if maxRetries := upstream.Spec().MaxRetries; maxRetries > 0 &&
			upstream.ShouldRetry(response.StatusCode) {
			switch {
			case retries < maxRetries:
				s.logger.Warn("Retry-After 超过上限，不再重试",
					"request_id", requestIDOf(ctx),
					"route", routeOf(route),
					"status", response.StatusCode,
					"retry_after", response.Header.Get("Retry-After"))
			case retries >= maxRetries:
				s.logger.Debug("渠道内重试次数已用尽",
					"request_id", requestIDOf(ctx),
					"route", routeOf(route),
					"status", response.StatusCode,
					"max_retries", maxRetries)
			}
		}
		if response.StatusCode >= http.StatusBadRequest {
			if upstream.ShouldFailover(response.StatusCode) && index < len(routes)-1 {
				lastMessage = readErrorBody(response)
				s.logger.Warn("上游拒绝请求，改试下一条路由", "request_id", requestIDOf(ctx), "route", routeOf(route), "status", response.StatusCode)
				accessOf(ctx).recordAttempt(route, response.StatusCode, FailureNone, retries, s.now().Sub(started))
				continue
			}
			// 不可重试的状态，或已经是最后一条候选：原样交给调用方。
			accessOf(ctx).recordAttempt(route, response.StatusCode, FailureNone, retries, s.now().Sub(started))
			accessOf(ctx).served(route.Channel)
			return response, prepared, route
		}
		if len(prepared.Diagnostics()) > 0 {
			s.logger.Debug("转换损耗",
				"request_id", requestIDOf(ctx),
				"route", routeOf(route),
				"count", len(prepared.Diagnostics()),
				"diagnostics", prepared.Diagnostics())
		}
		accessOf(ctx).recordAttempt(route, response.StatusCode, FailureNone, retries, s.now().Sub(started))
		accessOf(ctx).served(route.Channel)
		return response, prepared, route
	}
	if lastMessage == "" {
		lastMessage = "没有可用的候选路由"
	}
	// 候选全部试过：记下试了几条，渠道留空表示没有一条服务成功。
	accessOf(ctx).served("")
	s.writeError(ctx, writer, http.StatusBadGateway, "upstream_error", lastMessage)
	return nil, nil, config.Route{}
}

// forwardHeaders 组装这次上游请求的头，优先级从低到高是：客户端按渠道放行名单
// 转发的头、渠道 headers、插件按请求加的头、宿主 WithUpstreamHeader 设的头。也就是
// 「越具体越晚写」，与 channel/attempt 里先写 spec.Headers 再写请求级头是同一条口径。
//
// 名单里的头把客户端的所有值都带上：http.Header 支持多值，Cookie 这类重复字段的
// 头是合法写法。名单为空的渠道只带渠道头与宿主头，与没有放行功能时逐条相同。
func (s *Server) forwardHeaders(
	request *http.Request,
	channelName string,
	channelHeaders map[string]string,
	pluginHeaders map[string]string,
	host http.Header,
) http.Header {
	var out http.Header
	if allowed := s.passHeaders[channelName]; len(allowed) > 0 {
		for name, values := range request.Header {
			if !allowed[name] {
				continue
			}
			if out == nil {
				out = make(http.Header, len(allowed))
			}
			out[name] = append([]string(nil), values...)
		}
	}
	// 渠道头比客户端转发的具体，同名时渠道赢，所以在这里覆盖。
	for name, value := range channelHeaders {
		if out == nil {
			out = make(http.Header, len(channelHeaders))
		}
		out.Set(name, value)
	}
	// 插件按请求算出来的头比配置里的渠道头具体，同名时插件赢。
	for name, value := range pluginHeaders {
		if out == nil {
			out = make(http.Header, len(pluginHeaders))
		}
		out.Set(name, value)
	}
	// 宿主按请求设的最具体，最后写。
	for name, values := range host {
		if out == nil {
			out = make(http.Header, len(host))
		}
		out[name] = append([]string(nil), values...)
	}
	return out
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
	copyResponseHeaders(writer, response)
	if response.StatusCode >= http.StatusBadRequest {
		// 错误响应按原样透传：不同上游的错误形状差异很大，翻译只会丢掉排查线索。
		writer.WriteHeader(response.StatusCode)
		if _, err := io.Copy(writer, io.LimitReader(response.Body, s.maxBodyBytes)); err != nil {
			s.logger.Warn("写出上游错误响应失败", "request_id", requestIDOf(request.Context()), "error", err.Error())
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
		s.writeError(request.Context(), writer, http.StatusBadGateway, "upstream_error", fmt.Sprintf("读取上游响应失败: %v", err))
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
		s.logger.Warn("响应转换失败", "request_id", requestIDOf(request.Context()), "error", err.Error())
		s.writeError(request.Context(), writer, http.StatusBadGateway, "conversion_error", err.Error())
		return
	}
	out := converted.Body
	out = s.rewriteResponse(request.Context(), client, model, out)
	if !converted.Passthrough {
		writer.Header().Set("Content-Type", "application/json")
	}
	writer.WriteHeader(http.StatusOK)
	if _, err := writer.Write(out); err != nil {
		s.logger.Warn("写出响应失败", "request_id", requestIDOf(request.Context()), "error", err.Error())
	}
	// 用量交给访问日志那一行，这里不再单独记一条。
	accessOf(request.Context()).recordUsage(converted.Usage)
}

// rewriteRequest 依次调用启用插件的请求钩子。没有插件参与时不动原始字节。
//
// 返回值里的 headers 是插件加给上游的请求头，没有插件加过时为 nil。
func (s *Server) rewriteRequest(ctx context.Context, client dialect.Name, model string, stream bool, body []byte) ([]byte, string, map[string]string) {
	if !s.plugins.HasAny(plugin.HookRequest) {
		return body, model, nil
	}
	payload, err := json.Marshal(requestPayload{
		Dialect: string(client),
		Model:   model,
		Stream:  stream,
		Body:    json.RawMessage(body),
	})
	if err != nil {
		s.logger.Warn("构造请求钩子负载失败", "request_id", requestIDOf(ctx), "error", err.Error())
		return body, model, nil
	}
	outcome := s.plugins.Apply(ctx, plugin.HookRequest, payload, mergeRequest, nil, s.session())
	if !outcome.Changed() {
		return body, model, nil
	}
	var final requestPayload
	if err := json.Unmarshal(outcome.Data, &final); err != nil {
		s.logger.Warn("解析请求钩子输出失败", "request_id", requestIDOf(ctx), "plugins", outcome.Applied, "error", err.Error())
		return body, model, nil
	}
	updated := body
	if len(final.Body) > 0 {
		updated = final.Body
	}
	if final.Model != "" {
		model = final.Model
	}
	s.logger.Debug("请求钩子生效", "request_id", requestIDOf(ctx), "plugins", outcome.Applied, "model", model)
	return updated, model, final.Headers
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
		s.logger.Warn("构造响应钩子负载失败", "request_id", requestIDOf(ctx), "error", err.Error())
		return body
	}
	outcome := s.plugins.Apply(ctx, plugin.HookResponse, payload, mergeResponse, nil, s.session())
	if !outcome.Changed() {
		return body
	}
	var final responsePayload
	if err := json.Unmarshal(outcome.Data, &final); err != nil {
		s.logger.Warn("解析响应钩子输出失败", "request_id", requestIDOf(ctx), "plugins", outcome.Applied, "error", err.Error())
		return body
	}
	if len(final.Body) == 0 {
		return body
	}
	s.logger.Debug("响应钩子生效", "request_id", requestIDOf(ctx), "plugins", outcome.Applied)
	return final.Body
}

// session 返回本次调用的宿主环境。
func (s *Server) session() plugin.Session {
	return plugin.Session{Now: s.now(), Logger: s.logger}
}

// isEventStream 报告上游响应是否是 SSE。
func isEventStream(response *http.Response) bool {
	return strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream")
}

// copyResponseHeaders 复制需要透传的响应头。转换过的响应会在这之后覆盖
// Content-Type，错误响应则沿用上游给出的那一个。
func copyResponseHeaders(writer http.ResponseWriter, response *http.Response) {
	for _, name := range []string{"Content-Type", "Retry-After", "X-Request-Id"} {
		value := response.Header.Get(name)
		if value == "" {
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
