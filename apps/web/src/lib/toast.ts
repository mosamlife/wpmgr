// Thin re-export of `sonner` so the rest of the app uses a stable internal
// import surface and we can swap the toast implementation in one place. The
// <Toaster /> is mounted from App.tsx so every authenticated page can fire
// transient notifications without bespoke state management.
export { toast } from "sonner";
export { Toaster } from "sonner";
