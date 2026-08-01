import type { User } from "./types";

// The control API exposes user creation and mutation only under the
// root_security permission. In the built-in role model that permission belongs
// exclusively to owners; there is intentionally no separate self-update API.
export function hasRootSecurityPermission(user: Pick<User, "role"> | null | undefined) {
  return user?.role === "owner";
}
