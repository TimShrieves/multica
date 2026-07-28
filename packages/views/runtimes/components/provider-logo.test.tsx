import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { hasProviderLogo, ProviderLogo } from "./provider-logo";

describe("hasProviderLogo", () => {
  it("reports only the providers that render a mark of their own", () => {
    expect(hasProviderLogo("claude")).toBe(true);
    expect(hasProviderLogo("qwen")).toBe(true);
    // ProviderLogo answers these with the generic Monitor placeholder, which
    // identifies nothing — callers choosing an identity glyph must not use it.
    expect(hasProviderLogo("some-custom-backend")).toBe(false);
    expect(hasProviderLogo("")).toBe(false);
    expect(hasProviderLogo(null)).toBe(false);
    expect(hasProviderLogo(undefined)).toBe(false);
  });

  it("stays false for inherited Object properties", () => {
    expect(hasProviderLogo("toString")).toBe(false);
    expect(hasProviderLogo("constructor")).toBe(false);
  });
});

describe("ProviderLogo", () => {
  it("renders the dedicated Qwen Code mark", () => {
    const { container } = render(<ProviderLogo provider="qwen" className="runtime-logo" />);

    const logo = container.querySelector('img[aria-hidden="true"]');
    const logoSrc = decodeURIComponent(logo?.getAttribute("src") ?? "");

    expect(logo?.getAttribute("alt")).toBe("");
    expect(logoSrc).toContain("viewBox='0 0 141.38 140'");
    expect(logoSrc).toContain("fill='#6D44E8'");
    expect(logo?.classList.contains("runtime-logo")).toBe(true);
  });
});
