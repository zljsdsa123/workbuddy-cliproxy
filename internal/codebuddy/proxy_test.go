package codebuddy

import (
	"net/http"
	"testing"
)

// 生效代理的优先级与 CPA 语义保持一致：请求元数据 proxy_url → 凭据文件顶层
// proxy_url → 进程内缓存的系统代理 → 空（直连/继承环境代理）。

func TestParseStoredCapturesProxyURL(t *testing.T) {
	raw := []byte(`{
		"auth": {"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":"d"},
		"account": {"uid":"u1"},
		"proxy_url": "socks5://127.0.0.1:1080"
	}`)
	sa, err := ParseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sa.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Errorf("ProxyURL = %q, want the file's top-level proxy_url", sa.ProxyURL)
	}
}

func TestParseStoredWithoutProxyURL(t *testing.T) {
	raw := []byte(`{
		"auth": {"accessToken":"at","refreshToken":"rt","expiresAt":1},
		"account": {"uid":"u1"}
	}`)
	sa, err := ParseStored(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sa.ProxyURL != "" {
		t.Errorf("ProxyURL = %q, want empty", sa.ProxyURL)
	}
}

func resetSystemProxy(t *testing.T) {
	t.Helper()
	prev := SystemProxy()
	t.Cleanup(func() { CacheSystemProxy(prev) })
	CacheSystemProxy("")
}

func TestEffectiveProxyPrecedence(t *testing.T) {
	resetSystemProxy(t)
	fileProxy := &StoredAuth{ProxyURL: "http://file-proxy:8080"}
	plain := &StoredAuth{}

	cases := []struct {
		name string
		meta map[string]any
		sa   *StoredAuth
		want string
	}{
		{"meta wins", map[string]any{"proxy_url": "http://meta:8080"}, fileProxy, "http://meta:8080"},
		{"file wins", nil, fileProxy, "http://file-proxy:8080"},
		{"all empty", nil, plain, ""},
		{"direct override honored", map[string]any{"proxy_url": "direct"}, plain, "direct"},
		{"non-string metadata ignored", map[string]any{"proxy_url": 123}, fileProxy, "http://file-proxy:8080"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			CacheSystemProxy("") // 清系统代理，仅测前两级
			if got := EffectiveProxy(c.meta, c.sa); got != c.want {
				t.Errorf("EffectiveProxy = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("system proxy used when no per-auth override", func(t *testing.T) {
		CacheSystemProxy("http://system:8080")
		if got := EffectiveProxy(nil, plain); got != "http://system:8080" {
			t.Errorf("EffectiveProxy = %q, want system proxy", got)
		}
	})
}

// 空 proxy 必须与既有共享客户端完全一致（语义不变）。
func TestEmptyProxyDelegatesToShared(t *testing.T) {
	if got := JSONClient(""); got != SharedHTTPClient() {
		t.Error("JSONClient(\"\") != SharedHTTPClient()")
	}
	if got := Transport(""); got != SharedHTTPClient().Transport {
		t.Error("Transport(\"\") != SharedHTTPClient().Transport")
	}
}

func TestProxyClientKeepsCookieJar(t *testing.T) {
	// 代理客户端仅换传输，cookie jar 与总超时必须与共享客户端一致，
	// 避免会话/CSRF 行为因走代理而改变。
	shared := SharedHTTPClient()
	pc := JSONClient("http://127.0.0.1:1")
	if pc.Jar != shared.Jar {
		t.Error("proxy JSONClient must reuse the shared cookie jar")
	}
	if pc.Timeout != shared.Timeout {
		t.Errorf("proxy JSONClient timeout = %v, want %v", pc.Timeout, shared.Timeout)
	}
	if pc.Transport == shared.Transport {
		t.Error("proxy JSONClient must not reuse the shared default transport")
	}
}

func TestLoginClientIsolation(t *testing.T) {
	// 登录客户端永远用自己的 jar（state 间互不串扰），transport 按代理取。
	a := NewLoginClient("")
	b := NewLoginClient("")
	if a.Jar == b.Jar || a.Jar == SharedHTTPClient().Jar {
		t.Error("login clients must keep isolated cookie jars")
	}
	c := NewLoginClient("http://127.0.0.1:1")
	if c.Transport == SharedHTTPClient().Transport {
		t.Error("login client with a proxy must not use the shared default transport")
	}
}

func TestTransportCacheAndFallback(t *testing.T) {
	const proxy = "http://127.0.0.1:1" // 只建传输不连网，安全

	if a, b := Transport(proxy), Transport(proxy); a != b {
		t.Error("Transport should be cached per proxy")
	}

	// "direct" → 显式绕过代理的传输（Proxy=nil），且不同于默认共享传输。
	directTR, ok := Transport("direct").(*http.Transport)
	if !ok {
		t.Fatalf("direct transport is not *http.Transport")
	}
	if directTR.Proxy != nil {
		t.Error("direct transport must not consult any proxy")
	}
	if directTR == SharedHTTPClient().Transport {
		t.Error("direct transport should not be the shared default")
	}

	// 不支持的 scheme → 回退共享默认，绝不让坏代理打断整条链路。
	if got := Transport("ftp://nope"); got != SharedHTTPClient().Transport {
		t.Error("unsupported proxy should fall back to the shared default transport")
	}
}

func TestCacheSystemProxyTrims(t *testing.T) {
	resetSystemProxy(t)
	CacheSystemProxy("  http://system:8080  ")
	if got := SystemProxy(); got != "http://system:8080" {
		t.Errorf("SystemProxy = %q", got)
	}
	CacheSystemProxy("")
	if got := SystemProxy(); got != "" {
		t.Errorf("SystemProxy after clear = %q, want empty", got)
	}
}
