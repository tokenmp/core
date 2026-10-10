package relay

import (
	"context"
	"errors"

	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// MediaSource 是一次媒体解析请求的来源。
//
// URL 与 Data 只会有一个非空：URL 表示内容要自己去取，Data 是内联的 base64。
type MediaSource struct {
	// URL 是图片来源地址，需要取回内容。
	URL string
	// Data 是内联的 base64 内容，不含 data URL 前缀。
	Data string
	// MimeType 是内联内容声明的类型，可能为空。
	MimeType string
}

// identifierPrefixBytes 是标识里保留的内联数据长度。
const identifierPrefixBytes = 50

// IsURL 报告内容是否需要自己取回。
func (m MediaSource) IsURL() bool { return m.URL != "" }

// Identifier 返回可用于日志与报错的简短标识。
func (m MediaSource) Identifier() string {
	if m.IsURL() {
		return m.URL
	}
	if len(m.Data) > identifierPrefixBytes {
		return "base64:" + m.Data[:identifierPrefixBytes] + "..."
	}
	return "base64:" + m.Data
}

// MediaResolver 提供跨协议转换时需要下载图片与解析 data URL 的能力。
//
// 内核本身不发网络请求：两个回调都由宿主提供。缺少回调时，需要媒体解析的转换
// 会明确报错，而不是静默丢掉内容。
type MediaResolver struct {
	// GetBase64Data 按来源取回内容，返回 base64 数据与它的 MIME 类型。
	GetBase64Data func(ctx context.Context, source MediaSource) (base64Data string, mimeType string, err error)
	// DecodeBase64FileData 解析一段 data URL，返回它的 MIME 类型与 base64 数据。
	DecodeBase64FileData func(dataURL string) (mimeType string, base64Data string, err error)
}

// SetMediaResolver 注册媒体解析能力。
//
// 这是一个进程级的全局设置，只能在启动阶段调用一次。它与 RelayKit 的对应函数是
// 同一个开关，因此同一进程内只需要有一处设置。核心包提供这个入口，是为了让调用方
// 不必自己依赖 RelayKit。
func SetMediaResolver(resolver MediaResolver) {
	relayconvert.SetMediaResolver(relayconvert.MediaResolver{
		GetBase64Data: func(
			ctx context.Context,
			source types.FileSource,
			_ ...string,
		) (string, string, error) {
			if resolver.GetBase64Data == nil {
				return "", "", errMediaResolverMissing
			}
			return resolver.GetBase64Data(ctx, mediaSourceOf(source))
		},
		DecodeBase64FileData: func(dataURL string) (string, string, error) {
			if resolver.DecodeBase64FileData == nil {
				return "", "", errMediaResolverMissing
			}
			return resolver.DecodeBase64FileData(dataURL)
		},
	})
}

// mediaSourceOf 把 RelayKit 的文件来源整理成一次解析请求。
//
// RelayKit 的来源接口带着缓存与锁的管理方法，那些是它自己的记账，宿主的回调
// 只需要知道「是 URL 还是内联数据」这一个事实。
func mediaSourceOf(source types.FileSource) MediaSource {
	if source == nil {
		return MediaSource{}
	}
	if source.IsURL() {
		return MediaSource{URL: source.GetRawData()}
	}
	out := MediaSource{Data: source.GetRawData()}
	if inline, ok := source.(*types.Base64Source); ok {
		out.MimeType = inline.MimeType
	}
	return out
}

// errMediaResolverMissing 表示宿主没有提供这个方向的能力。
var errMediaResolverMissing = errors.New("relay: 媒体解析回调未注册")
