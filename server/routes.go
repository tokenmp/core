package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/dialect"
)

// 端点上与协议无关的固定路径。
const (
	healthPath = "/healthz"
	modelsPath = "/v1/models"
	// geminiPrefix 是 Gemini 端点前缀，其后是模型名与动作。
	geminiPrefix = "/v1beta/models/"
)

// routes 组装路由表，最外层包上访问日志。宿主中间件由 New 装在这层的外面。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, s.handleHealth)
	mux.HandleFunc(modelsPath, s.handleModels)
	mux.HandleFunc(dialect.OpenAIChat.LocalPath(), s.exact(dialect.OpenAIChat))
	mux.HandleFunc(dialect.OpenAIResponses.LocalPath(), s.exact(dialect.OpenAIResponses))
	mux.HandleFunc(dialect.ClaudeMessages.LocalPath(), s.exact(dialect.ClaudeMessages))
	// 计数端点单列一条路由而不是并进上面那条：它要额外把上游路径补成
	// /v1/messages/count_tokens，走 exact 那条通用路径会打到消息端点上。
	mux.HandleFunc(dialect.ClaudeCountTokensPath, s.handleCountTokens)
	mux.HandleFunc(geminiPrefix, s.handleGemini)
	return s.withAccessLog(mux)
}

// exact 返回一个只接受 POST 的处理器。
func (s *Server) exact(client dialect.Name) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if !s.requirePost(writer, request) {
			return
		}
		s.forward(writer, request, client, "", false, endpoint{})
	}
}

// handleGemini 处理 Gemini 的两条端点：模型名与动作都在路径上。
func (s *Server) handleGemini(writer http.ResponseWriter, request *http.Request) {
	if !s.requirePost(writer, request) {
		return
	}
	rest := strings.TrimPrefix(request.URL.Path, geminiPrefix)
	actionIndex := strings.LastIndex(rest, ":")
	if actionIndex <= 0 || actionIndex == len(rest)-1 {
		s.writeError(request.Context(), writer, http.StatusNotFound, "路由不存在",
			fmt.Sprintf("路径 %s 缺少模型名或动作", request.URL.Path))
		return
	}
	model := rest[:actionIndex]
	action := rest[actionIndex+1:]
	var stream bool
	switch action {
	case "generateContent":
		stream = false
	case "streamGenerateContent":
		stream = true
	default:
		s.writeError(request.Context(), writer, http.StatusNotFound, "路由不存在",
			fmt.Sprintf("未知动作 %s，可用动作为 generateContent 与 streamGenerateContent", action))
		return
	}
	s.forward(writer, request, dialect.GeminiGenerate, model, stream, endpoint{})
}

// handleHealth 是存活探测端点。
func (s *Server) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		s.writeError(request.Context(), writer, http.StatusMethodNotAllowed, "方法不允许", "该端点只接受 GET")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte(`{"status":"ok"}`))
}

// handleModels 按 OpenAI 的形状返回本地可路由的模型名。
func (s *Server) handleModels(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		s.writeError(request.Context(), writer, http.StatusMethodNotAllowed, "方法不允许", "该端点只接受 GET")
		return
	}
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	response := struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}{Object: "list"}
	for _, rule := range s.config.Models {
		response.Data = append(response.Data, modelEntry{ID: rule.Match, Object: "model", OwnedBy: rule.Channel})
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		s.logger.Warn("写出模型列表失败", "request_id", requestIDOf(request.Context()), "error", err.Error())
	}
}

// requirePost 校验方法，不通过时已写出响应。
func (s *Server) requirePost(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method == http.MethodPost {
		return true
	}
	writer.Header().Set("Allow", http.MethodPost)
	s.writeError(request.Context(), writer, http.StatusMethodNotAllowed, "方法不允许", "该端点只接受 POST")
	return false
}

// readBody 读取请求体并做大小限制。
func (s *Server) readBody(writer http.ResponseWriter, request *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, s.maxBodyBytes))
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			s.writeError(request.Context(), writer, http.StatusRequestEntityTooLarge, "请求体过大",
				fmt.Sprintf("请求体超过 %d 字节上限", s.maxBodyBytes))
			return nil, false
		}
		s.writeError(request.Context(), writer, http.StatusBadRequest, "请求体不可读", err.Error())
		return nil, false
	}
	return body, true
}

// writeError 按 OpenAI 的错误形状写出错误响应。
//
// 错误形状只统一本地产生的这一种；上游返回的错误按原样透传，不做跨方言翻译。
// 带 ctx 是为了让「写出错误响应失败」这条日志也能带上 request_id。
func (s *Server) writeError(ctx context.Context, writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	}
	if err := json.NewEncoder(writer).Encode(payload); err != nil {
		s.logger.Warn("写出错误响应失败", "request_id", requestIDOf(ctx), "error", err.Error())
	}
}

// routeOf 返回一条路由的可读描述。
func routeOf(route config.Route) string {
	return fmt.Sprintf("%s(%s)", route.Channel, route.Dialect)
}
