// @vitest-environment jsdom

import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import type { RuntimeUsage } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import zhRuntimes from "../../../locales/zh-Hans/runtimes.json";

vi.mock("@multica/core/runtimes/custom-pricing-store", () => {
  const pricingState = { pricings: {} };
  const useCustomPricingStore = Object.assign(
    (selector?: (state: typeof pricingState) => unknown) =>
      selector ? selector(pricingState) : pricingState,
    { getState: () => pricingState },
  );

  return {
    useCustomPricingStore,
    getCustomPricing: () => undefined,
  };
});

import { ActivityHeatmap } from "./activity-heatmap";

const USAGE: RuntimeUsage[] = [
  {
    runtime_id: "runtime-1",
    date: "2026-08-12",
    provider: "anthropic",
    model: "claude-sonnet-4-6",
    input_tokens: 1_000,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
    cost_usd_ticks: 20_000_000_000,
    uncosted_input_tokens: 0,
    uncosted_output_tokens: 0,
    uncosted_cache_read_tokens: 0,
    uncosted_cache_write_tokens: 0,
  },
];

describe("ActivityHeatmap localization", () => {
  beforeAll(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-12T12:00:00Z"));
  });

  afterAll(() => {
    vi.useRealTimers();
  });

  it("renders chart chrome, dates, and tooltips in the selected locale", () => {
    const { container } = render(
      <I18nProvider
        locale="zh-Hans"
        resources={{ "zh-Hans": { runtimes: zhRuntimes } }}
      >
        <ActivityHeatmap usage={USAGE} tz="Asia/Shanghai" />
      </I18nProvider>,
    );

    expect(screen.getByText("最活跃日期")).toBeTruthy();
    expect(screen.getByText("最活跃星期几")).toBeTruthy();
    expect(screen.getByText("最不活跃星期几")).toBeTruthy();
    expect(screen.getByText("8月12日")).toBeTruthy();
    expect(screen.getAllByText("周三").length).toBeGreaterThan(0);
    expect(screen.getByText(/天总计$/)).toBeTruthy();
    expect(screen.queryByText("Busiest day")).toBeNull();

    const titles = Array.from(container.querySelectorAll("title"), (title) =>
      title.textContent?.trim(),
    );
    expect(titles.some((title) => title?.endsWith(": 无活动"))).toBe(true);
  });
});
