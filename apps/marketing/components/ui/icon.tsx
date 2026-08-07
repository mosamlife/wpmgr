import {
  Minus,
  Lock,
  Layers,
  Info,
  Database,
  Activity,
  ArrowRight,
  AlertTriangle,
  BadgeCheck,
  BarChart2,
  Briefcase,
  Check,
  ChevronDown,
  Container as ContainerIcon,
  Copy,
  Cpu,
  DatabaseBackup,
  DatabaseZap,
  Eraser,
  EyeOff,
  FileBadge,
  FileLock2,
  FileScan,
  FileSearch,
  Gauge,
  GitFork,
  GitPullRequest,
  Globe,
  Handshake,
  HardDrive,
  HelpCircle,
  ImageDown,
  ImageOff,
  Infinity as InfinityIcon,
  KeyRound,
  KeySquare,
  Laptop,
  LayoutDashboard,
  LayoutGrid,
  LockKeyhole,
  Mail,
  MailCheck,
  Menu,
  Monitor,
  Moon,
  Network,
  PlusCircle,
  RefreshCw,
  Replace,
  RotateCcw,
  Scale,
  Scissors,
  ScrollText,
  Server,
  ServerCog,
  ShieldAlert,
  ShieldCheck,
  ShieldOff,
  ShieldPlus,
  Smartphone,
  Star,
  Sun,
  ToggleLeft,
  TrendingUp,
  Type,
  Undo2,
  Upload,
  UserPlus,
  Users,
  X,
  Zap,
  type LucideIcon,
} from "lucide-react";

// Explicit registry: icons referenced by string from content data stay
// tree-shakeable and type-checked. One stroke family, one weight.
const REGISTRY: Record<string, LucideIcon> = {
  Minus,
  Lock,
  Layers,
  Info,
  Database,
  AlertTriangle,
  Activity,
  ArrowRight,
  BadgeCheck,
  BarChart2,
  Briefcase,
  Check,
  ChevronDown,
  ContainerIcon,
  Copy,
  Cpu,
  DatabaseBackup,
  DatabaseZap,
  Eraser,
  EyeOff,
  FileBadge,
  FileLock2,
  FileScan,
  FileSearch,
  Gauge,
  GitFork,
  GitPullRequest,
  Globe,
  Handshake,
  HardDrive,
  HelpCircle,
  ImageDown,
  ImageOff,
  Infinity: InfinityIcon,
  KeyRound,
  KeySquare,
  Laptop,
  LayoutDashboard,
  LayoutGrid,
  LockKeyhole,
  Mail,
  MailCheck,
  Menu,
  Monitor,
  Moon,
  Network,
  PlusCircle,
  RefreshCw,
  Replace,
  RotateCcw,
  Scale,
  Scissors,
  ScrollText,
  Server,
  ServerCog,
  ShieldAlert,
  ShieldCheck,
  ShieldOff,
  // ShieldUser was removed in lucide-react; ShieldPlus is the closest equivalent.
  ShieldUser: ShieldPlus,
  Smartphone,
  Star,
  Sun,
  ToggleLeft,
  TrendingUp,
  Type,
  Undo2,
  Upload,
  UserPlus,
  Users,
  X,
  Zap,
};

/** GitHub mark. lucide-react dropped brand glyphs; hand-carried as filled
 *  path inheriting currentColor. */
function GithubMark({ size = 20, className }: { size?: number; className?: string }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="currentColor"
      className={className}
      aria-hidden
    >
      <path d="M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23a11.5 11.5 0 0 1 3-.405c1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12" />
    </svg>
  );
}

export function Icon({
  name,
  size = 20,
  className,
  strokeWidth = 1.75,
}: {
  name: string;
  size?: number;
  className?: string;
  strokeWidth?: number;
}) {
  if (name === "Github") return <GithubMark size={size} className={className} />;
  const Cmp = REGISTRY[name] ?? HelpCircle;
  return <Cmp size={size} strokeWidth={strokeWidth} className={className} aria-hidden />;
}
