// The tool registry. The left rail renders one icon per entry; each tool owns
// its own top bar. Today there is one tool (Trace); the rail exists so a second
// (monitoring, later) is a one-line addition.
export interface Tool {
  id: string
  title: string
  path: string
  icon: string
}

export const TOOLS: Tool[] = [
  {
    id: 'agents',
    title: 'Agents',
    path: '/agents',
    icon: `<svg viewBox="0 0 24 24" width="20" height="20" fill="none"
             stroke="currentColor" stroke-width="1.8"
             stroke-linecap="round" stroke-linejoin="round">
             <rect x="3" y="4" width="18" height="5" rx="1"/>
             <rect x="3" y="12" width="18" height="5" rx="1"/>
             <circle cx="7" cy="6.5" r="0.6" fill="currentColor"/>
             <circle cx="7" cy="14.5" r="0.6" fill="currentColor"/>
           </svg>`,
  },
  {
    id: 'trace',
    title: 'Trace',
    path: '/trace',
    icon: `<svg viewBox="0 0 24 24" width="20" height="20" fill="none"
             stroke="currentColor" stroke-width="1.8"
             stroke-linecap="round" stroke-linejoin="round">
             <circle cx="4" cy="12" r="1.6"/>
             <path d="M6.4 12h3l2-5 3 12 2.2-7H20"/>
           </svg>`,
  },
]
