/**
 * Workaround for google-antigravity 429 RESOURCE_EXHAUSTED.
 *
 * The literal tag name "<system-conventions>" correlates with Cloud Code
 * Assist rejecting requests. The server-side mechanism is unknown; what is
 * observed is that renaming the tag (while preserving the instructions)
 * restores normal operation.
 *
 * See: https://github.com/can1357/oh-my-pi/issues/11794
 */
export default function (pi: { on: (e: string, h: (ev: unknown) => unknown) => void }) {
  pi.on("before_provider_request", async (event) => {
    if (!event || typeof event !== "object") return undefined;
    if (!("payload" in event)) return undefined;
    const { payload } = event;

    if (!payload || typeof payload !== "object") return undefined;
    if (!("request" in payload)) return undefined;
    const { request } = payload;

    if (!request || typeof request !== "object") return undefined;
    if (!("systemInstruction" in request)) return undefined;
    const { systemInstruction } = request;

    if (!systemInstruction || typeof systemInstruction !== "object") return undefined;
    if (!("parts" in systemInstruction)) return undefined;
    const { parts } = systemInstruction;

    if (!Array.isArray(parts)) return undefined;

    for (const part of parts) {
      if (!part || typeof part !== "object") continue;
      if (!("text" in part) || typeof part.text !== "string") continue;
      if (part.text.includes("system-conventions")) {
        part.text = part.text.replace(/system-conventions/g, "conventions");
      }
    }

    return payload;
  });
}
