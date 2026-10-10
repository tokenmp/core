// 示例插件：在请求末尾加一条系统消息，在响应上打一个标记。
//
// 契约见 plugin-api/v1/v1.d.ts。钩子返回 null 表示不改写。

export const meta = {
  key: "echo",
  name: "回显",
  version: "0.1.0",
  apiVersion: "v1",
  description: "把插件标记写进请求，把插件名写进响应",
};

export function onRequest(input) {
  if (!input.body || !Array.isArray(input.body.messages)) return null;
  input.body.messages.push({
    role: "system",
    content: "tag=" + host.config.tag,
  });
  host.log("已写入标记");
  return { body: input.body };
}

export function onResponse(input) {
  if (!input.body || typeof input.body !== "object") return null;
  input.body.plugin = host.plugin.key;
  return { body: input.body };
}
