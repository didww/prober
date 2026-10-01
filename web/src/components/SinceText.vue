<script setup lang="ts">
// How long ago an ISO timestamp was, kept current by the shared clock. The
// text is a computed, so a tick re-renders this only when the text changes:
// with minute precision, about once a minute.
import { computed } from 'vue'
import { useNow } from '../clock'
import { since } from '../settings'

const props = defineProps<{ at?: string; minutes?: boolean; suffix?: string }>()
const now = useNow()
const text = computed(() => (props.at ? since(props.at, now.value, props.minutes) + (props.suffix ?? '') : '—'))
</script>

<template>{{ text }}</template>
