<script setup lang="ts">
import { useData } from 'vitepress'
import { onMounted, ref, useId, watch } from 'vue'

const props = defineProps<{ code: string }>()
const { isDark } = useData()
const svg = ref('')
const source = decodeURIComponent(props.code)
const id = `mermaid-${useId()}`

async function render() {
  // Loaded on demand: only pages with a diagram pay for the library.
  const { default: mermaid } = await import('mermaid')
  mermaid.initialize({ startOnLoad: false, theme: isDark.value ? 'dark' : 'default' })
  svg.value = (await mermaid.render(id, source)).svg
}

onMounted(render)
watch(isDark, render)
</script>

<template>
  <div v-if="svg" class="mermaid-diagram" v-html="svg" />
  <pre v-else>{{ source }}</pre>
</template>

<style scoped>
.mermaid-diagram {
  margin: 16px 0;
  overflow-x: auto;
}

/* Mermaid sizes label boxes outside the page, so the page's paragraph
   spacing must not apply inside them or the text is clipped. */
.mermaid-diagram :deep(p) {
  margin: 0;
  line-height: 1.5;
}
</style>
