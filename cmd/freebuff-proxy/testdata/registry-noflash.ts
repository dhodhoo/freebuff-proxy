// Minimal FREE_MODE_AGENT_MODELS fixture WITHOUT deepseek/deepseek-v4-flash
// (that model is never in this catalog), used to pin probeModel's
// "else first model" fallback: the alphabetical first model must be
// returned when deepseek-v4-flash is absent.
export const FREE_MODE_AGENT_MODELS: Record<string, Set<string>> = {
  'fable-agent': new Set(['anthropic/claude-fable-5']),
  'zeta-agent': new Set(['zeta/model-one']),
}
