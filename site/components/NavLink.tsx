"use client";
import Link from "next/link";
import { usePathname } from "next/navigation";

export function NavLink({ slug, title }: { slug: string; title: string }) {
  const href = `/docs/${slug}`;
  const active = usePathname() === href;
  return (
    <Link
      className={`doc-nav-link${active ? " is-active" : ""}`}
      href={href}
      aria-current={active ? "page" : undefined}
    >
      {title}
    </Link>
  );
}
