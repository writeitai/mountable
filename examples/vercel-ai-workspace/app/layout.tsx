import type { ReactNode } from "react";
export const metadata = { title: "mountable.io · AI SDK file tools" };
export default function Layout({ children }: { children: ReactNode }) {
  return <html lang="en"><body style={{ fontFamily: "system-ui", maxWidth: 720, margin: "4rem auto", padding: "0 1.5rem", lineHeight: 1.6 }}>{children}</body></html>;
}
