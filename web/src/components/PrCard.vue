<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import type { PRView } from '../api'
import { prStatusLabel } from '../status'

const props = defineProps<{
  pr: PRView
}>()

const status = computed(() => prStatusLabel(props.pr))
</script>

<template>
  <article class="pr-card">
    <header class="pr-card__head">
      <span class="pr-card__repo">{{ pr.repo }} #{{ pr.number }}</span>
      <span class="badge pr-card__status" :class="`badge--${status.tone}`">{{ status.tag }}</span>
    </header>

    <RouterLink class="pr-card__link" :to="`/prs/${pr.id}`">{{ pr.title }}</RouterLink>

    <p class="pr-card__meta">
      <span class="pr-card__author">por {{ pr.author_login }}</span>
    </p>

    <p v-if="pr.validations.total > 0" class="pr-card__validations">
      {{ pr.validations.passed }}/{{ pr.validations.total }} validaciones correctas
    </p>

    <footer class="pr-card__footer">
      <span class="pr-card__action">{{ status.action }}</span>
      <a class="pr-card__github" :href="pr.html_url" target="_blank" rel="noreferrer">
        Ver en GitHub
      </a>
    </footer>
  </article>
</template>
