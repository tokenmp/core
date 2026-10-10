/**
 * TokenMP core 插件契约 v1。
 *
 * 插件是一个单文件同步 ECMAScript 模块，导出 meta 与若干钩子函数。宿主每次调用
 * 都在全新的运行环境里求值一次模块，因此顶层代码每次都会重跑，请把它写轻。
 *
 * 宿主不提供的能力：网络请求、文件系统、require、动态 import、eval 与
 * Function 构造器、进程环境变量。钩子必须是同步函数，返回 Promise 会被拒绝。
 *
 * 钩子返回 null 或 undefined 表示不改写，宿主沿用上一个插件的输出。
 * 钩子抛异常或超时表示本次改写作废，转发照常继续。
 */

/** 插件的自述信息，必须导出，且 key 与插件所在目录名一致。 */
export interface PluginMeta {
  /** 插件标识，须与目录名一致。 */
  key: string;
  /** 展示名。 */
  name: string;
  /** 插件自己的版本号。 */
  version: string;
  /** 契约版本，目前只支持 "v1"。 */
  apiVersion: "v1";
  /** 可选说明。 */
  description?: string;
}

/** host.plugin 提供的自述信息。 */
export interface HostPlugin {
  key: string;
  name: string;
  version: string;
  apiVersion: string;
}

/** 宿主对象，模块内以全局变量 host 使用。 */
export interface Host {
  /** 配置里该插件的 config 段。未配置时是空对象。 */
  readonly config: Record<string, unknown>;
  /** 插件的自述信息。 */
  readonly plugin: HostPlugin;
  /** 当前时间，Unix 秒。fixture 回放时是固定值。 */
  now(): number;
  /** 写一条宿主日志。 */
  log(message: string): void;
}

declare global {
  // eslint-disable-next-line no-var
  var host: Host;
}

/** onRequest 的输入。 */
export interface RequestPayload {
  /** 客户端方言：openai、openai_responses、claude、gemini 之一。 */
  dialect: string;
  /** 客户端请求的模型名。Gemini 取自路径。 */
  model: string;
  /** 客户端是否要求流式。 */
  stream: boolean;
  /** 请求体，已解析成 JSON 值。 */
  body: unknown;
}

/** onRequest 的输出。未给出的字段保持原值。 */
export interface RequestRewrite {
  /** 替换模型名，会改变选路。 */
  model?: string;
  /** 替换请求体。 */
  body?: unknown;
}

/** onResponse 的输入。 */
export interface ResponsePayload {
  /** 客户端方言。 */
  dialect: string;
  /** 本次请求的模型名。 */
  model: string;
  /** 已经成形的响应体，已解析成 JSON 值。 */
  body: unknown;
}

/** onResponse 的输出。 */
export interface ResponseRewrite {
  /** 替换响应体。 */
  body?: unknown;
}

/** onEvent 的输入。 */
export interface EventPayload {
  /** 客户端方言。 */
  dialect: string;
  /** SSE 的事件名，可能为空。 */
  event: string;
  /** 事件的 JSON 负载。 */
  data: unknown;
}

/** onEvent 的输出。 */
export interface EventRewrite {
  /** 替换事件名。 */
  event?: string;
  /** 替换事件负载。 */
  data?: unknown;
}

/** 一条流式事件。 */
export interface StreamEvent {
  /** SSE 的事件名，可能为空。 */
  event?: string;
  /** 事件的 JSON 负载。 */
  data: unknown;
}

/** onStreamEnd 的输入。 */
export interface StreamEndPayload {
  /** 客户端方言。 */
  dialect: string;
  /** 本次请求的模型名。 */
  model: string;
  /** 累计用量，可能为 null。 */
  usage: unknown | null;
  /** 收尾阶段要补发的分片。 */
  events: StreamEvent[];
}

/** onStreamEnd 的输出。 */
export interface StreamEndRewrite {
  /** 替换补发的分片。 */
  events?: StreamEvent[];
}

/** 插件模块的形状。meta 必填，四个钩子按需导出。 */
export interface PluginModule {
  meta: PluginMeta;
  onRequest?(input: RequestPayload): RequestRewrite | null | undefined;
  onResponse?(input: ResponsePayload): ResponseRewrite | null | undefined;
  onEvent?(input: EventPayload): EventRewrite | null | undefined;
  onStreamEnd?(input: StreamEndPayload): StreamEndRewrite | null | undefined;
}
