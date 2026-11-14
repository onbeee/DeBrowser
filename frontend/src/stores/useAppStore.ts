import { create } from "zustand";
import type { Status } from "../../bindings/github.com/onbeee/DeBrowser/internal/service";

export type PageKey =
  | "overview"
  | "instances"
  | "proxyconfig"
  | "kernels"
  | "bookmarks"
  | "sync"
  | "backup"
  | "diagnostics"
  | "settings";

export type Theme = "dark" | "light" | "warm";

export interface LogLine {
  id: number;
  time: string;
  level: "info" | "error";
  message: string;
}

interface AppState {
  page: PageKey;
  status: Status | null;
  statusError: string | null;
  logs: LogLine[];
  theme: Theme;
  setPage: (page: PageKey) => void;
  setStatus: (status: Status) => void;
  setStatusError: (message: string | null) => void;
  log: (level: "info" | "error", message: string) => void;
  clearLogs: () => void;
  setTheme: (theme: Theme) => void;
}

let nextLogId = 1;

/** Apply a theme immediately by switching the design-token set. */
export function applyTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme;
}

export const useAppStore = create<AppState>((set) => ({
  page: "overview",
  status: null,
  statusError: null,
  logs: [],
  theme: "dark",
  setPage: (page) => set({ page }),
  setStatus: (status) => set({ status, statusError: null }),
  setStatusError: (message) => set({ statusError: message }),
  log: (level, message) =>
    set((state) => ({
      logs: [
        ...state.logs.slice(-199),
        { id: nextLogId++, time: new Date().toLocaleTimeString("zh-CN"), level, message },
      ],
    })),
  clearLogs: () => set({ logs: [] }),
  setTheme: (theme) => {
    applyTheme(theme);
    set({ theme });
  },
}));
