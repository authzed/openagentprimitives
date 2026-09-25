import { describe, it, expect, afterEach } from "vitest";
import { readBootstrap } from "./bootstrap";

afterEach(() => {
  document.body.innerHTML = "";
});

describe("readBootstrap", () => {
  it("parses JSON from the #ap-bootstrap script element", () => {
    document.body.innerHTML =
      '<script id="ap-bootstrap" type="application/json">{"hello":"world","n":1}</script>';
    expect(readBootstrap<{ hello: string; n: number }>()).toEqual({
      hello: "world",
      n: 1,
    });
  });

  it("returns an empty object when the element is absent", () => {
    expect(readBootstrap()).toEqual({});
  });

  it("does not execute embedded markup (treats content as data)", () => {
    document.body.innerHTML =
      '<script id="ap-bootstrap" type="application/json">{"x":"\\u003c/script\\u003e"}</script>';
    expect(readBootstrap<{ x: string }>()).toEqual({ x: "</script>" });
  });
});
