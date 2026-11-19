<script setup lang="ts">
function login(): void {
  window.location.assign('/auth/login')
}
</script>

<template>
  <section class="auth-page">
    <div class="auth-card">
      <span class="brand__mark brand__mark--large" aria-hidden="true">QA</span>
      <h1 class="auth-card__title">QA Control Center</h1>
      <p class="auth-card__subtitle">Revisión de código asistida por IA para tus repositorios.</p>

      <button type="button" class="button button--primary button--block" @click="login">
        Entrar con GitHub
      </button>

      <p class="auth-card__hint">El primer usuario que entre será administrador de la instancia.</p>
    </div>
  </section>
</template>
