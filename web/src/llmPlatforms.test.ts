import { describe, expect, it } from 'vitest'
import { llmPlatforms } from './llmPlatforms'

describe('llmPlatforms', () => {
  it('lists fifteen platforms with unique provider ids', () => {
    expect(llmPlatforms).toHaveLength(15)
    const ids = llmPlatforms.map((platform) => platform.id)
    expect(new Set(ids).size).toBe(ids.length)
    for (const platform of llmPlatforms) {
      expect(platform.id.trim()).not.toBe('')
      expect(platform.name.trim()).not.toBe('')
    }
  })

  it('gives every platform labelled models with unique ids', () => {
    for (const platform of llmPlatforms) {
      expect(platform.models.length).toBeGreaterThan(0)
      const ids = platform.models.map((model) => model.id)
      expect(new Set(ids).size).toBe(ids.length)
      for (const model of platform.models) {
        expect(model.id.trim()).not.toBe('')
        expect(model.label.trim()).not.toBe('')
      }
    }
  })

  it('offers OpenCode Go as a provider separate from OpenCode Models', () => {
    const platform = llmPlatforms.find((entry) => entry.id === 'opencode-go')
    expect(platform?.name).toBe('OpenCode Go')
    expect(platform?.models.map((model) => model.id)).toContain('deepseek-v4.1-flash')
  })
})
