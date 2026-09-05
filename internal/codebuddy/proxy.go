package codebuddy

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

// 出站代理。CPA 的宿主把"系统代理"（全局 proxy-url）带在带 Host 的请求上
// （auth.parse / auth.login.* / auth.refresh），并把宿主元数据（含每凭据的
// proxy_url）合并进凭据文件顶层（mergedStorageJSON）。本包把它们撮合成一个
// 进程内的"每凭据生效代理"，让 CodeBuddy 上游的每条出网腿（对话 / 刷新 /
// 登录 / 积分探测）都按 CPA 的语义走同一个代理：
//
//	1. 请求元数据里的 proxy_url（host 管理的运行时值，最高优先）
//	2. 凭据文件顶层 proxy_url（认证文件单独配置代理）
//	3. 进程内缓存的系统代理（从带 Host 的请求上捕获，供 executor 等无 Host 的路径回退）
//	4. 空 → 保持默认直连/继承环境代理（现状）
//
// "direct"/"none" 显式绕过全局与环境代理（proxyutil.ModeDirect）。
// 传输复用 CPA 自带的 sdk/proxyutil：与宿主其余出网路径同一套 socks5/http/https
// 语义，避免在插件里再造一套轮子。
//
// CodeBuddy 只有一个进程级共享客户端（带 cookie jar），不像 trae 有独立的
// 流式客户端，所以这里按单种传输缓存；代理客户端保留共享 jar 与总超时，
// 语义只多了"走代理"，其余与改造前一致。

// ---------------------------------------------------------------------------
// 系统代理缓存：executor.execute / quota 探测等请求不带 HostConfigSummary，
// 全局 proxy-url 只在带 Host 的请求上可见，这里缓存最后一次看到的值供它们回退。
// ---------------------------------------------------------------------------

var (
	systemProxyMu sync.RWMutex
	systemProxy   string
)

// CacheSystemProxy 记录一次带 Host 的请求里看到的系统代理（可能为空）。
func CacheSystemProxy(p string) {
	systemProxyMu.Lock()
	systemProxy = strings.TrimSpace(p)
	systemProxyMu.Unlock()
}

// SystemProxy 返回最近一次从 Host 请求上捕获的系统代理；没有则返回空串。
func SystemProxy() string {
	systemProxyMu.RLock()
	defer systemProxyMu.RUnlock()
	return systemProxy
}

// ---------------------------------------------------------------------------
// 每代理传输缓存
// ---------------------------------------------------------------------------

var (
	proxyMu   sync.Mutex
	transport = map[string]http.RoundTripper{}
)

// Transport 返回 proxy 对应的出站传输。proxy 为空 → 共享默认传输（现状）。
func Transport(proxy string) http.RoundTripper {
	if strings.TrimSpace(proxy) == "" {
		return SharedHTTPClient().Transport
	}
	return transportFor(proxy)
}

// JSONClient 返回 proxy 对应的 JSON/对话客户端：保留共享 cookie jar 与总超时
// （与改造前 SharedHTTPClient 唯一区别是出站走代理）。proxy 为空 → 与
// SharedHTTPClient() 完全一致。
func JSONClient(proxy string) *http.Client {
	shared := SharedHTTPClient()
	if strings.TrimSpace(proxy) == "" {
		return shared
	}
	return &http.Client{
		Timeout:   shared.Timeout,
		Jar:       shared.Jar,
		Transport: Transport(proxy),
	}
}

// transportFor 返回按 proxy 缓存的传输。代理配置非法/不支持时回退到共享默认
// 传输并记一条警告——宁可保持现状直连，也不让一条错误代理把请求全部打挂。
func transportFor(proxy string) http.RoundTripper {
	proxyMu.Lock()
	defer proxyMu.Unlock()
	if tr := transport[proxy]; tr != nil {
		return tr
	}
	tr, err := buildProxyTransport(proxy)
	if err != nil {
		log.Printf("codebuddy: invalid proxy %q, falling back to direct: %v", proxyutil.Redact(proxy), err)
		return SharedHTTPClient().Transport
	}
	transport[proxy] = tr
	return tr
}

// buildProxyTransport 用 CPA 的 proxyutil 构造出站传输：支持
// socks5/socks5h/http/https，及 "direct"/"none"（绕过全局与环境代理）。
// 顺带把连接池参数调成与共享客户端一致（MaxIdleConnsPerHost/IdleConnTimeout）。
func buildProxyTransport(proxy string) (http.RoundTripper, error) {
	tr, _, err := proxyutil.BuildHTTPTransport(proxy)
	if err != nil {
		return nil, err
	}
	if tr == nil {
		return nil, fmt.Errorf("proxy %q built an empty transport", proxyutil.Redact(proxy))
	}
	tr.IdleConnTimeout = 90 * time.Second
	tr.MaxIdleConnsPerHost = 5
	return tr, nil
}

// MetaProxy 读取请求元数据（AuthMetadata/Metadata）里的每凭据 proxy_url。
func MetaProxy(meta map[string]any) string {
	if v, ok := meta["proxy_url"]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// EffectiveProxy 计算一次出网调用的生效代理：请求元数据 → 凭据文件顶层
// proxy_url → 进程内系统代理。任一环节出现非空（含 "direct"/"none"）即生效；
// 全空 → 直连/继承环境代理（与 CPA 的 ModeInherit 语义一致）。
func EffectiveProxy(meta map[string]any, sa *StoredAuth) string {
	if p := MetaProxy(meta); p != "" {
		return p
	}
	if sa != nil {
		if p := strings.TrimSpace(sa.ProxyURL); p != "" {
			return p
		}
	}
	return SystemProxy()
}
