package relay

import (
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/tokenmp/core/dialect"
)

// 这一组类型是内核公开面的一部分，刻意与 RelayKit 的同名类型分开。
//
// 取舍写在 docs/relay.md 里：诊断、转换报告、损耗策略、推理状态都由本包定义，
// 因为它们字段少、含义稳定，而且 CLI 要照着自己想要的格式打印。RelayKit 的
// DTO 与 dto.Usage 则直接出现在公开面上，理由是镜像 19 个字段的用量结构只会带来
// 漂移，见同一个文档的「与 RelayKit 的关系」。

// Severity 是诊断的级别。
type Severity string

const (
	// SeverityWarning 表示转换完成但丢了信息。
	SeverityWarning Severity = "warning"
	// SeverityError 表示转换丢了会影响执行语义的信息。
	SeverityError Severity = "error"
)

// Diagnostic 是一条转换损耗。
type Diagnostic struct {
	// Code 是机器可读的原因标识。
	Code string
	// Path 是出问题的字段路径，可能为空。
	Path string
	// Message 是给人看的说明。
	Message string
	// Severity 是级别。
	Severity Severity
	// From 与 To 是这一跳的源方言与目标方言。
	From dialect.Name
	To   dialect.Name
}

// Quality 是转换质量的等级。
type Quality string

const (
	// QualityGood 表示两种协议的核心结构较接近。
	QualityGood Quality = "good"
	// QualityFair 表示主要内容能转，协议独有字段以诊断形式报出。
	QualityFair Quality = "fair"
	// QualityDiscouraged 表示需要经过中间协议转换，语义损失风险更高。
	QualityDiscouraged Quality = "discouraged"
)

// Step 是一次转换跳。
type Step struct {
	// Converter 是这一跳所用的转换器标识。
	Converter string
	// From 与 To 是这一跳的源方言与目标方言。
	From dialect.Name
	To   dialect.Name
}

// Report 说明一次转换实际是怎么完成的。
type Report struct {
	// Converter 是所选转换器的标识。
	Converter string
	// Quality 是转换质量的等级。
	Quality Quality
	// Steps 是实际经过的转换跳；直接转换只有一跳。
	Steps []Step
	// From 与 To 是整个转换的源方言与目标方言。
	From dialect.Name
	To   dialect.Name
}

// LossPolicy 决定请求转换遇到工具语义损耗时怎么处理。
type LossPolicy string

const (
	// LossPolicyAllow 是默认值：照常完成转换，每条损耗都记进诊断。
	LossPolicyAllow LossPolicy = "allow"
	// LossPolicySafe 在 error 级诊断时把请求转换判为失败。
	LossPolicySafe LossPolicy = "safe"
	// LossPolicyStrict 在任何诊断下都把请求转换判为失败。
	LossPolicyStrict LossPolicy = "strict"
)

// Reasoning 是模型名尾缀解析出的推理设置。
type Reasoning struct {
	// Mode 是推理模式，例如 thinking 或 nothinking。
	Mode string
	// Effort 是推理强度，例如 low、medium、high。
	Effort string
	// BudgetTokens 是推理预算的 token 数。
	BudgetTokens *int
	// IncludeThoughts 表示是否要求把推理内容写进响应。
	IncludeThoughts *bool
}

// reportOf 把 RelayKit 的请求转换结果整理成报告。
func reportOfRequest(result *relayconvert.RequestResult) Report {
	if result == nil {
		return Report{}
	}
	return Report{
		Converter: result.Converter,
		Quality:   Quality(result.Quality),
		Steps:     requestSteps(result.Steps),
		From:      dialectOf(result.From),
		To:        dialectOf(result.To),
	}
}

// reportOfResponse 把 RelayKit 的响应转换结果整理成报告。
func reportOfResponse(result *relayconvert.ResponseResult) Report {
	if result == nil {
		return Report{}
	}
	return Report{
		Converter: result.Converter,
		Quality:   Quality(result.Quality),
		Steps:     responseSteps(result.Steps),
		From:      dialectOf(result.From),
		To:        dialectOf(result.To),
	}
}

// requestSteps 转换请求转换的步骤列表。
func requestSteps(steps []relayconvert.RequestStep) []Step {
	if len(steps) == 0 {
		return nil
	}
	out := make([]Step, 0, len(steps))
	for _, step := range steps {
		out = append(out, Step{
			Converter: step.Converter,
			From:      dialectOf(step.From),
			To:        dialectOf(step.To),
		})
	}
	return out
}

// responseSteps 转换响应转换的步骤列表。
func responseSteps(steps []relayconvert.ResponseStep) []Step {
	if len(steps) == 0 {
		return nil
	}
	out := make([]Step, 0, len(steps))
	for _, step := range steps {
		out = append(out, Step{
			Converter: step.Converter,
			From:      dialectOf(step.From),
			To:        dialectOf(step.To),
		})
	}
	return out
}

// requestDiagnostics 转换请求阶段的诊断。
func requestDiagnostics(result *relayconvert.RequestResult) []Diagnostic {
	if result == nil {
		return nil
	}
	return diagnostics(result.Diagnostics)
}

// responseDiagnostics 转换响应阶段的诊断。
func responseDiagnostics(result *relayconvert.ResponseResult) []Diagnostic {
	if result == nil {
		return nil
	}
	return diagnostics(result.Diagnostics)
}

// diagnostics 逐个转换诊断。
func diagnostics(items []types.ConversionDiagnostic) []Diagnostic {
	if len(items) == 0 {
		return nil
	}
	out := make([]Diagnostic, 0, len(items))
	for _, item := range items {
		out = append(out, Diagnostic{
			Code:     item.Code,
			Path:     item.Path,
			Message:  item.Message,
			Severity: Severity(item.Severity),
			From:     dialectOf(item.From),
			To:       dialectOf(item.To),
		})
	}
	return out
}

// reasoningOf 转换推理状态。未识别出尾缀时返回 nil。
func reasoningOf(state *dto.ReasoningConversionState) *Reasoning {
	if state == nil {
		return nil
	}
	return &Reasoning{
		Mode:            state.Mode,
		Effort:          state.Effort,
		BudgetTokens:    state.BudgetTokens,
		IncludeThoughts: state.IncludeThoughts,
	}
}

// relayFormatOf 把方言映射成 RelayKit 的格式。
//
// 这个映射只在这里用得到，因此留在本包而不是暴露成 dialect 的公开函数：
// 映射的目标类型属于 RelayKit，暴露出去等于让调用方也依赖它的类型词汇。
func relayFormatOf(name dialect.Name) types.RelayFormat {
	switch name {
	case dialect.OpenAIChat:
		return types.RelayFormatOpenAI
	case dialect.OpenAIResponses:
		return types.RelayFormatOpenAIResponses
	case dialect.ClaudeMessages:
		return types.RelayFormatClaude
	case dialect.GeminiGenerate:
		return types.RelayFormatGemini
	default:
		return ""
	}
}

// dialectOf 把 RelayKit 的格式映射回方言。不认识的格式返回空。
func dialectOf(format types.RelayFormat) dialect.Name {
	switch format {
	case types.RelayFormatOpenAI:
		return dialect.OpenAIChat
	case types.RelayFormatOpenAIResponses:
		return dialect.OpenAIResponses
	case types.RelayFormatClaude:
		return dialect.ClaudeMessages
	case types.RelayFormatGemini:
		return dialect.GeminiGenerate
	default:
		return ""
	}
}

// reasoningStateOf 把推理设置还原成 RelayKit 的状态类型，供转换上下文使用。
func reasoningStateOf(reasoning *Reasoning) *dto.ReasoningConversionState {
	if reasoning == nil {
		return nil
	}
	return &dto.ReasoningConversionState{
		Mode:            reasoning.Mode,
		Effort:          reasoning.Effort,
		BudgetTokens:    reasoning.BudgetTokens,
		IncludeThoughts: reasoning.IncludeThoughts,
	}
}
