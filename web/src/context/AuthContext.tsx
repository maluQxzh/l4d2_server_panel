import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createContext, useContext, useEffect, useMemo, type ReactNode } from "react";
import { api, ApiError, onSessionExpired } from "../lib/api";
import type { User } from "../lib/types";

interface AuthValue {
  user: User | null;
  loading: boolean;
  authenticated: boolean;
  activationRequired: boolean;
  bootstrapRequired: boolean;
  bootstrapKeyConfigured: boolean;
  bootstrapOwner: typeof api.bootstrapOwner;
  refreshBootstrap: () => Promise<boolean>;
  login: typeof api.login;
  completeActivation: typeof api.completeActivation;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  useEffect(() => onSessionExpired(() => {
    // Cancel pending responses before removing cached resources so an old
    // request cannot repopulate the next account's view.
    void queryClient.cancelQueries({ predicate: (query) => query.queryKey[0] !== "bootstrap-status" });
    queryClient.setQueryData(["session"], null);
    queryClient.removeQueries({ predicate: (query) => !["session", "bootstrap-status"].includes(String(query.queryKey[0])) });
    queryClient.getMutationCache().clear();
    sessionStorage.removeItem("l4d2-event-cursor");
  }), [queryClient]);
  const session = useQuery({
    queryKey: ["session"],
    queryFn: api.me,
    retry: (count, error) => !(error instanceof ApiError && error.status === 401) && count < 1,
    staleTime: 60_000,
    refetchInterval: (query) => query.state.data?.user ? 60_000 : false,
    refetchOnWindowFocus: true,
  });
  const bootstrapStatus = useQuery({
    queryKey: ["bootstrap-status"],
    queryFn: api.bootstrapStatus,
    retry: 1,
    staleTime: 30_000,
  });

  const value = useMemo<AuthValue>(() => ({
    user: session.data?.user || null,
    loading: session.isPending || bootstrapStatus.isPending,
    authenticated: Boolean(session.data?.user),
    activationRequired: session.data?.user?.account_state === "pending_activation",
    bootstrapRequired: Boolean(bootstrapStatus.data?.required),
    bootstrapKeyConfigured: Boolean(bootstrapStatus.data?.key_configured),
    bootstrapOwner: async (payload) => {
      const result = await api.bootstrapOwner(payload);
      queryClient.setQueryData(["session"], result);
      queryClient.setQueryData(["bootstrap-status"], { required: false, key_configured: false });
      return result;
    },
    refreshBootstrap: async () => Boolean((await bootstrapStatus.refetch()).data?.required),
    login: async (credentials) => {
      await queryClient.cancelQueries({ queryKey: ["session"] });
      const result = await api.login(credentials);
      queryClient.setQueryData(["session"], result);
      return result;
    },
    completeActivation: async (newPassword) => {
      const result = await api.completeActivation(newPassword);
      queryClient.setQueryData(["session"], result);
      return result;
    },
    logout: async () => {
      const pending = api.logout();
      void queryClient.cancelQueries({ predicate: (query) => query.queryKey[0] !== "bootstrap-status" });
      queryClient.setQueryData(["session"], null);
      queryClient.removeQueries({ predicate: (query) => !["session", "bootstrap-status"].includes(String(query.queryKey[0])) });
      await pending;
    },
  }), [bootstrapStatus, queryClient, session.data, session.isPending]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  const value = useContext(AuthContext);
  if (!value) throw new Error("useAuth 必须在 AuthProvider 内使用");
  return value;
}
