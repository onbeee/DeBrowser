import { create } from "zustand";
import type { Status } from "../../bindings/github.com/onbeee/DeBrowser/internal/service";

export type PageKey =
  | "instances"
  | "proxyconfig"
  | "kernels"
  | "bookmarks"
  | "sync"
  | "datamanagement"
  | "settings";

export type Theme = "dark" | "light" | "warm";

interface AppState {
  page: PageKey;
  status: Status | null;
  statusError: string | null;
  theme: Theme;
  setPage: (page: PageKey) => void;
  setStatus: (status: Status) => void;
  setStatusError: (message: string | null) => void;
  setTheme: (theme: Theme) => void;
}

/** Apply a theme immediately by switching the design-token set. */
export function applyTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme;
}

export const useAppStore = create<AppState>((set) => ({
  page: "instances",
  status: null,
  statusError: null,
  // First launch defaults to the light theme; a saved user
  // choice is loaded from the backend settings right after start
  // and replaces this fallback.
  theme: "light",
  setPage: (page) => set({ page }),
  setStatus: (status) => set({ status, statusError: null }),
  setStatusError: (message) => set({ statusError: message }),
  setTheme: (theme) => {
    applyTheme(theme);
    set({ theme });
  },
}));
