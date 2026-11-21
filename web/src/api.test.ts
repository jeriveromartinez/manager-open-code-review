import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from './api'

describe('api.llmModels', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('posts the provider and key to the models endpoint', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ models: [{ id: 'm', name: 'M' }] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    vi.stubGlobal('fetch', fetchMock)

    await expect(api.llmModels('anthropic', 'sk-ant')).resolves.toEqual({
      models: [{ id: 'm', name: 'M' }],
    })
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/llm-profiles/models',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ provider: 'anthropic', api_key: 'sk-ant' }),
      }),
    )
  })
})
