// The tool registry. The left rail renders one icon per entry, in this order;
// each tool owns its own top bar. Adding a tool (monitoring, later) is a
// one-line addition here plus a route in main.ts.
export interface Tool {
  id: string
  title: string
  path: string
  icon: string
}

export const TOOLS: Tool[] = [
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
  {
    // SIP OPTIONS reachability over a selectable transport.
    id: 'sip',
    title: 'SIP',
    path: '/sip',
    icon: `<svg viewBox="0 0 24 24" width="20" height="20" fill="none"
             stroke="currentColor" stroke-width="1.8"
             stroke-linecap="round" stroke-linejoin="round">
             <path d="M6 4h9a3 3 0 0 1 0 6H9v10"/>
             <path d="M9 10v4"/>
           </svg>`,
  },
  {
    // Resolve a name with each agent's own resolver: A, AAAA and the SIP SRVs.
    id: 'dns',
    title: 'DNS',
    path: '/dns',
    icon: `<svg viewBox="0 0 24 24" width="20" height="20" fill="none"
             stroke="currentColor" stroke-width="1.8"
             stroke-linecap="round" stroke-linejoin="round">
             <circle cx="12" cy="12" r="8.5"/>
             <path d="M3.5 12h17"/>
             <path d="M12 3.5c2.6 2.4 3.9 5.2 3.9 8.5s-1.3 6.1-3.9 8.5c-2.6-2.4-3.9-5.2-3.9-8.5s1.3-6.1 3.9-8.5z"/>
           </svg>`,
  },
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
]
