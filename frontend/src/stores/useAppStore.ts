import { create } from "zustand";
import type { Status } from "../../bindings/github.com/onbeee/DeBrowser/internal/service";

export type PageKey =
  | "overview"
  | "instances"
  | "proxies"
  | "kernels"
  | "bookmarks"
  | "sync"
  | "backup"
  | "settings";

export interface LogLine {
  id: number;
  time: string;
  level: "info" | "error";
  message: string;
}

interface AppState {
  page: PageKey;
  consoleOpen: boolean;
  status: Status | null;
  statusError: string | null;
  logs: LogLine[];
  setPage: (page: PageKey) => void;
  toggleConsole: () => void;
  setConsoleOpen: (open: boolean) => void;
  setStatus: (status: Status) => void;
  setStatusError: (message: string | null) => void;
  log: (level: "info" | "error", message: string) => void;
  clearLogs: () => void;
}

let nextLogId = 1;

export const useAppStore = create<AppState>((set) => ({
  page: "overview",
  consoleOpen: false,
  status: null,
  statusError: null,
  logs: [],
  setPage: (page) => set({ page }),
  toggleConsole: () => set((state) => ({ consoleOpen: !state.consoleOpen })),
  setConsoleOpen: (open) => set({ consoleOpen: open }),
  setStatus: (status) => set({ status, statusError: null }),
  setStatusError: (message) => set({ statusError: message }),
  log: (level, message) =>
    set((state) => ({
      logs: [
        ...state.logs.slice(-199),
        { id: nextLogId++, time: new Date().toLocaleTimeString(), level, message },
      ],
    })),
  clearLogs: () => set({ logs: [] }),
}));
