import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createContext, useContext, useMemo, type ReactNode } from "react";
import { api, ApiError } from "../lib/api";
import type { User } from "../lib/types";

interface AuthValue {
  user: User | null;
  loading: boolean;
  authenticated: boolean;
  bootstrapRequired: boolean;
  bootstrapOwner: typeof api.bootstrapOwner;
  refreshBootstrap: () => Promise<boolean>;
  login: typeof api.login;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const session = useQuery({
    queryKey: ["session"],
    queryFn: api.me,
    retry: (count, error) => !(error instanceof ApiError && error.status === 401) && count < 1,
    staleTime: 60_000,
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
    bootstrapRequired: Boolean(bootstrapStatus.data?.required),
    bootstrapOwner: async (payload) => {
      const result = await api.bootstrapOwner(payload);
      queryClient.setQueryData(["session"], result);
      queryClient.setQueryData(["bootstrap-status"], { required: false });
      return result;
    },
    refreshBootstrap: async () => Boolean((await bootstrapStatus.refetch()).data?.required),
    login: async (credentials) => {
      const result = await api.login(credentials);
      queryClient.setQueryData(["session"], result);
      return result;
    },
    logout: async () => {
      await api.logout();
      queryClient.setQueryData(["session"], null);
      queryClient.removeQueries({ predicate: (query) => query.queryKey[0] !== "session" });
    },
  }), [bootstrapStatus, queryClient, session.data, session.isPending]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  const value = useContext(AuthContext);
  if (!value) throw new Error("useAuth 必须在 AuthProvider 内使用");
  return value;
}
