export type LLMPlatformModel = {
  id: string
  label: string
}

export type LLMPlatform = {
  id: string
  name: string
  models: LLMPlatformModel[]
}

// Provider ids are the ones OpenCode resolves from models.dev. The first model
// of each platform is the one preselected when the platform is chosen.
export const llmPlatforms: LLMPlatform[] = [
  {
    id: 'openai',
    name: 'OpenAI',
    models: [
      { id: 'gpt-6.2-sol', label: 'GPT-6.2 Sol' },
      { id: 'gpt-6.2-astra', label: 'GPT-6.2 Astra' },
      { id: 'gpt-5.4-nano', label: 'GPT-5.4 nano' },
    ],
  },
  {
    id: 'anthropic',
    name: 'Anthropic',
    models: [
      { id: 'claude-sonnet-5-5', label: 'Claude Sonnet 5.5' },
      { id: 'claude-opus-5-5', label: 'Claude Opus 5.5' },
      { id: 'claude-haiku-5-5', label: 'Claude Haiku 5.5' },
    ],
  },
  {
    id: 'google',
    name: 'Google Gemini',
    models: [
      { id: 'gemini-4-carbon', label: 'Gemini 4 Carbon' },
      { id: 'gemini-4-neon', label: 'Gemini 4 Neon' },
      { id: 'gemini-3.8-flash', label: 'Gemini 3.8 Flash' },
    ],
  },
  {
    id: 'openrouter',
    name: 'OpenRouter',
    models: [
      { id: 'openrouter/auto', label: 'Automático (OpenRouter elige)' },
      { id: 'anthropic/claude-sonnet-5.5', label: 'Claude Sonnet 5.5' },
      { id: 'google/gemini-3.7-flash', label: 'Gemini 3.7 Flash' },
    ],
  },
  {
    id: 'groq',
    name: 'Groq',
    models: [
      { id: 'openai/gpt-oss-120b', label: 'GPT-OSS 120B' },
      { id: 'qwen/qwen3.8-27b', label: 'Qwen3.8 27B' },
      { id: 'llama-3.3-70b-versatile', label: 'Llama 3.3 70B' },
    ],
  },
  {
    id: 'mistral',
    name: 'Mistral',
    models: [
      { id: 'mistral-large-latest', label: 'Mistral Large' },
      { id: 'devstral-medium-latest', label: 'Devstral 2' },
      { id: 'mistral-medium-2604', label: 'Mistral Medium 3.5' },
    ],
  },
  {
    id: 'deepseek',
    name: 'DeepSeek',
    models: [
      { id: 'deepseek-v4-pro', label: 'DeepSeek V4 Pro' },
      { id: 'deepseek-v4-flash', label: 'DeepSeek V4 Flash' },
    ],
  },
  {
    id: 'xai',
    name: 'xAI',
    models: [
      { id: 'grok-4.7', label: 'Grok 4.7' },
      { id: 'grok-4.6', label: 'Grok 4.6' },
      { id: 'grok-4.5', label: 'Grok 4.5' },
    ],
  },
  {
    id: 'togetherai',
    name: 'Together AI',
    models: [
      { id: 'moonshotai/Kimi-K3.1-Pro', label: 'Kimi K3.1 Pro' },
      { id: 'deepseek-ai/DeepSeek-V4-Pro-0813', label: 'DeepSeek V4 Pro' },
      { id: 'MiniMaxAI/MiniMax-M3', label: 'MiniMax M3' },
    ],
  },
  {
    id: 'fireworks-ai',
    name: 'Fireworks',
    models: [
      {
        id: 'accounts/fireworks/routers/deepseek-flash-latest',
        label: 'DeepSeek Flash',
      },
      { id: 'accounts/fireworks/routers/glm-latest', label: 'GLM' },
      { id: 'accounts/fireworks/routers/kimi-k3-fast', label: 'Kimi K3 Fast' },
    ],
  },
]
