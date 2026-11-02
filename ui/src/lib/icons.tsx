/**
 * Icon system for agent-orca UI.
 *
 * Replaces emoji with consistent, stroke-weight-matched SVG icons from
 * Lucide React. All icons render at the same 1.5px stroke for visual harmony
 * (better-ui §14, §15). Icons use `currentColor` so they inherit text color.
 *
 * Usage:
 *   import { Icon, ICON } from '../lib/icons'
 *   <Icon icon={ICON.bot} size={20} />
 *
 * The `STATUS_ICON` map provides phase-specific status icons for StatusBadge.
 */
import React, { type CSSProperties } from 'react'
import {
  AlertTriangle,
  ArrowRight,
  Bell,
  Book,
  Bot,
  Check,
  ChevronRight,
  ChevronDown,
  ClipboardList,
  Clock,
  Code,
  Copy,
  Database,
  DollarSign,
  Hash,
  ThumbsDown,
  ThumbsUp,
  Headphones,
  Home,
  History,
  MessageCircle,
  MoreHorizontal,
  Octagon,
  Pause,
  Plus,
  Power,
  RefreshCw,
  RotateCw,
  Search,
  Send,
  Settings,
  ShoppingBag,
  Shuffle,
  Square,
  Star,
  Terminal,
  ToolCase,
  Zap,
  X,
  Sun,
  Moon,
  BarChart3,
  Plug,
  Brain,
  Library,
  Wrench,
  Activity,
  HelpCircle,
  LoaderCircle,
  Play,
  ShieldCheck,
} from 'lucide-react'

export type IconComponent = typeof Bot // structural type: any Lucide icon

/** Icon component with consistent sizing, color inheritance, and reduced-motion support. */
interface IconProps {
  icon: IconComponent
  size?: number
  strokeWidth?: number
  color?: string
  style?: CSSProperties
  ariaHidden?: boolean
}

export function Icon({ icon: IconCmp, size = 14, strokeWidth = 1.5, color, style, ariaHidden = true }: IconProps) {
  return (
    <IconCmp
      size={size}
      strokeWidth={strokeWidth}
      color={color}
      aria-hidden={ariaHidden}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        justifyContent: 'center',
        flexShrink: 0,
        // Transition opacity so icon-only buttons can fade between states (better-ui §16)
        transitionProperty: 'opacity, color',
        transitionDuration: '0.15s',
        transitionTimingFunction: 'ease',
        ...style,
      }}
    />
  )
}

/**
 * Semantic icon mapping — replaces every emoji used in the codebase.
 * Named by purpose, not appearance, so a redesign never touches component code.
 */
export const ICON = {
  // Agents & resources
  agent: Bot,
  tool: Wrench,
  mcpserver: Plug,
  modelprovider: Brain,
  knowledgebase: Book,
  modelselector: Shuffle,
  deployment: Zap,
  workflow: RefreshCw,
  runs: Play,

  // History
  history: History,
  approval: ShieldCheck,

  // Agent templates
  templates: {
    support: Headphones,
    research: Search,
    code: Code,
    data: BarChart3,
  },

  // Actions
  create: Plus,
  close: X,
  copy: Copy,
  send: Send,
  stop: Square,
  cancel: X,
  save: Check,

  // UI states
  output: Star,
  cost: DollarSign,
  chart: BarChart3,
  success: Check,
  check: Check,
  error: X,
  warning: AlertTriangle,
  timer: Clock,
  retry: RotateCw,
  tokens: Hash,
  home: Home,
  trace: ClipboardList,
  settings: Settings,
  thought: Activity,
  routed: ArrowRight,
  thumbsUp: ThumbsUp,
  thumbsDown: ThumbsDown,
  event: Bell,
  parseError: Octagon,

  // Activity / lifecycle
  pause: Pause,
  power: Power,
  search: Search,
  marketplace: ShoppingBag,
  sun: Sun,
  moon: Moon,
  shuffle: Shuffle,
  database: Database,

  // Chevrons
  chevronRight: ChevronRight,
  chevronDown: ChevronDown,

  // Misc
  library: Library,
  message: MessageCircle,
  help: HelpCircle,
  spinner: LoaderCircle,
  placeholder: MoreHorizontal,
  token: Terminal,
} as const

/**
 * Phase → status icon mapping for StatusBadge.
 * Each status gets a distinct icon so the badge is meaningful without color alone
 * (better-accessibility §9 — status needs a redundant cue).
 */
export const STATUS_ICON: Record<string, IconComponent> = {
  Succeeded: Check,
  Running: LoaderCircle,
  Creating: LoaderCircle,
  Failed: X,
  Cancelled: X,
  Pending: Clock,
  Paused: Pause,
  HandedOff: Shuffle,
  Skipped: Play,
  WaitingForInput: HelpCircle,
}

export function StatusIcon({ phase, size = 10, spinning = false }: { phase: string; size?: number; spinning?: boolean }) {
  const IconCmp = STATUS_ICON[phase] ?? null
  if (!IconCmp) return null
  return (
    <Icon
      icon={IconCmp}
      size={size}
      strokeWidth={1.5}
      color={STATUS_COLOR_HEX[phase]}
      ariaHidden={true}
      style={{
        animation: spinning ? 'aoPulse 1.5s ease infinite' : undefined,
        willChange: spinning ? 'transform, opacity' : undefined,
      }}
    />
  )
}

// Map phases to their color for the status icon (reuses phase color values)
const STATUS_COLOR_HEX: Record<string, string> = {
  Succeeded: '#22c55e',
  Running: '#3b82f6',
  Creating: '#3b82f6',
  Failed: '#ef4444',
  Cancelled: '#ef4444',
  Pending: '#f59e0b',
  Paused: '#64748b',
  HandedOff: '#a78bfa',
  Skipped: '#a78bfa',
  WaitingForInput: '#f59e0b',
}
