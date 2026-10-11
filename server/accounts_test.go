package server_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/server"
)

// TestAccountsExpandIntoRoutableChannels 核对顶层账号展开出的渠道名在装配之后真的可用，
// 并且每条渠道带的是自己那把凭据。
//
// 这是 config 与 server 之间唯一的接缝：config.Resolve 给出的候选名必须是 channel.Set
// 里的键，对不上时请求会以「渠道 X 不存在」结束。假上游对第一把凭据回 429，一次请求就
// 同时覆盖了展开、逐账号凭据、换账号降级，以及 Outcome 交出的是真正服务的那条渠道。
func TestAccountsExpandIntoRoutableChannels(t *testing.T) {
	t.Setenv("TOKENMP_TEST_KEY_A", "key-a")
	t.Setenv("TOKENMP_TEST_KEY_B", "key-b")

	upstream, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "Bearer key-a" {
			writeJSON(writer, http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
			return
		}
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, outcomes := newOutcomeServer(t, `
accounts:
  - {name: team-a, key_env: TOKENMP_TEST_KEY_A}
  - {name: team-b, key_env: TOKENMP_TEST_KEY_B}
channels:
  - name: pool
    dialect: openai
    base_url: "`+upstreamServer.URL+`"
    auth: {style: bearer, accounts: [team-a, team-b]}
models:
  - {match: "client-model", channel: pool}
`)

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("第一个账号被限流后应换第二个账号，实际状态码 %d: %s", status, body)
	}

	// 凭据来自展开出的渠道，不是基础渠道：两把各出现一次，顺序按账号的声明序。
	upstream.mu.Lock()
	credentials := make([]string, 0, len(upstream.requests))
	for _, item := range upstream.requests {
		credentials = append(credentials, strings.TrimPrefix(item.Header.Get("Authorization"), "Bearer "))
	}
	upstream.mu.Unlock()
	if len(credentials) != 2 || credentials[0] != "key-a" || credentials[1] != "key-b" {
		t.Fatalf("两个账号应各收到一次带自己凭据的请求，实际 %v", credentials)
	}

	outcome := outcomes.wait(t, 1)[0]
	if outcome.Channel != "pool/team-b" {
		t.Fatalf("Outcome 应交出真正服务的那条展开渠道，实际 %q", outcome.Channel)
	}
	if len(outcome.Attempts) != 2 ||
		outcome.Attempts[0].Channel != "pool/team-a" || outcome.Attempts[0].Status != http.StatusTooManyRequests ||
		outcome.Attempts[1].Channel != "pool/team-b" || outcome.Attempts[1].Status != http.StatusOK {
		t.Fatalf("尝试记录应按账号展开名逐条交出，实际 %+v", outcome.Attempts)
	}
}

// TestHandBuiltConfigWithAccountsIsRoutable 覆盖程序化构造配置这条路。
//
// 这样构造出来的 Config 没走过 Parse，没有填默认值也没有展开，装配层要自己补上；
// 同时也校对装配不去改宿主手里那份配置。
func TestHandBuiltConfigWithAccountsIsRoutable(t *testing.T) {
	t.Setenv("TOKENMP_TEST_KEY_A", "key-a")

	upstream, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	cfg := &config.Config{
		Accounts: []config.Account{{Name: "team-a", KeyEnv: "TOKENMP_TEST_KEY_A"}},
		Channels: []config.Channel{{
			Name:    "pool",
			Dialect: "openai",
			BaseURL: upstreamServer.URL,
			Auth:    config.Auth{Style: "bearer", Accounts: []string{"team-a"}},
		}},
		Models: []config.Model{{Match: "client-model", Channel: "pool"}},
	}

	handler, err := server.FromConfigWith(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("装配服务失败: %v", err)
	}
	front := httptest.NewServer(handler.Handler())
	t.Cleanup(front.Close)

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("手工构造的配置应能转发，实际状态码 %d: %s", status, body)
	}
	if got := upstream.last(t).Header.Get("Authorization"); got != "Bearer key-a" {
		t.Fatalf("展开出的渠道应用账号的凭据，实际 %q", got)
	}

	// 装配不改宿主手里那份：账号、基础渠道名与账号引用都原样。
	if len(cfg.Accounts) != 1 || cfg.Channels[0].Name != "pool" || len(cfg.Channels[0].Auth.Accounts) != 1 {
		t.Fatalf("装配不应改动宿主的配置: %+v", cfg)
	}
}
