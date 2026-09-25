import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { renderHook, act, cleanup } from "@testing-library/react";
import { useAwayNotifications } from "./useAwayNotifications";

// MockNotification stands in for the Web Notifications API (absent in jsdom).
class MockNotification {
  static permission: NotificationPermission = "granted";
  static requestPermission = vi.fn(async () => "granted" as NotificationPermission);
  static instances: MockNotification[] = [];
  onclick: (() => void) | null = null;
  constructor(public title: string, public options?: { body?: string }) {
    MockNotification.instances.push(this);
  }
  close() {}
}

beforeEach(() => {
  MockNotification.instances = [];
  MockNotification.permission = "granted";
  MockNotification.requestPermission = vi.fn(async () => "granted" as NotificationPermission);
  vi.stubGlobal("Notification", MockNotification);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("useAwayNotifications", () => {
  it("bumps unread and fires a Notification when a reply arrives while away", () => {
    const { result } = renderHook(() => useAwayNotifications(false));
    act(() => result.current.notify("deploy questions", "here is the answer"));
    expect(result.current.unread).toBe(1);
    expect(MockNotification.instances).toHaveLength(1);
    expect(MockNotification.instances[0].title).toBe("deploy questions");
    expect(MockNotification.instances[0].options?.body).toBe("here is the answer");
  });

  it("does NOT notify or bump unread while the tab is active", () => {
    const { result } = renderHook(() => useAwayNotifications(true));
    act(() => result.current.notify("t", "b"));
    expect(result.current.unread).toBe(0);
    expect(MockNotification.instances).toHaveLength(0);
  });

  it("clears unread when the tab becomes active again", () => {
    const { result, rerender } = renderHook(({ a }) => useAwayNotifications(a), {
      initialProps: { a: false },
    });
    act(() => result.current.notify("t", "b"));
    expect(result.current.unread).toBe(1);
    rerender({ a: true });
    expect(result.current.unread).toBe(0);
  });

  it("bumps unread but skips the popup when permission is not granted", () => {
    MockNotification.permission = "denied";
    const { result } = renderHook(() => useAwayNotifications(false));
    act(() => result.current.notify("t", "b"));
    expect(result.current.unread).toBe(1);
    expect(MockNotification.instances).toHaveLength(0);
  });

  it("requests permission once, only when currently 'default'", () => {
    MockNotification.permission = "default";
    const { result } = renderHook(() => useAwayNotifications(false));
    act(() => result.current.requestPermission());
    act(() => result.current.requestPermission());
    expect(MockNotification.requestPermission).toHaveBeenCalledTimes(1);
  });
});
