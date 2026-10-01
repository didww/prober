// A shared one-second clock for relative times ("12m ago"). A component that
// shows one reads the clock itself, so a tick re-renders just that component,
// not the page around it. The clock runs only while something uses it.
import { onBeforeUnmount, onMounted, ref, type Ref } from 'vue'

const now = ref(Date.now())
let users = 0
let timer = 0

export function useNow(): Ref<number> {
  onMounted(() => {
    if (users++ === 0) {
      now.value = Date.now()
      timer = window.setInterval(() => (now.value = Date.now()), 1000)
    }
  })
  onBeforeUnmount(() => {
    if (--users === 0) clearInterval(timer)
  })
  return now
}
