// Sparkline is a plain SVG polyline rather than a recharts <LineChart>. The
// component header explains why (one Redux store per recharts root, up to 100
// of them on a fleet screen). These tests pin the behaviour that had to
// survive the rewrite: the accessibility wrapper, the >=2-point guard, the
// gap handling, and the geometry the recharts version drew.

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { Sparkline, type SparklineDatum } from "./sparkline";

function polylinePoints(container: HTMLElement): Array<[number, number]> {
  const line = container.querySelector("polyline");
  const raw = line?.getAttribute("points") ?? "";
  return raw
    .split(" ")
    .filter(Boolean)
    .map((pair) => {
      const [x, y] = pair.split(",").map(Number);
      return [x!, y!] as [number, number];
    });
}

describe("Sparkline -- accessibility and shape", () => {
  it("keeps the role=img wrapper and its label", () => {
    render(<Sparkline data={[1, 5, 3]} ariaLabel="Uptime trend" />);

    const img = screen.getByRole("img", { name: "Uptime trend" });
    expect(img).toBeInTheDocument();
    // The inner SVG must not be announced separately.
    expect(img.querySelector("svg")).toHaveAttribute("aria-hidden", "true");
  });

  it("renders one polyline and no recharts machinery", () => {
    const { container } = render(<Sparkline data={[1, 5, 3]} />);

    expect(container.querySelectorAll("polyline")).toHaveLength(1);
    expect(container.querySelector(".recharts-wrapper")).toBeNull();
    expect(container.querySelector(".recharts-surface")).toBeNull();
  });

  it("uses the tone token for the stroke, never a hex value", () => {
    const { container } = render(<Sparkline data={[1, 5, 3]} tone="destructive" />);

    expect(container.querySelector("polyline")).toHaveAttribute(
      "stroke",
      "var(--color-destructive)",
    );
  });
});

describe("Sparkline -- the >=2 real points rule", () => {
  it("renders a spacer and no line for an empty series", () => {
    const { container } = render(<Sparkline data={[]} />);

    expect(container.querySelector("polyline")).toBeNull();
    expect(container.querySelector("span")).toHaveAttribute("aria-hidden", "true");
  });

  it("renders a spacer for a single point rather than a misleading dot", () => {
    const { container } = render(<Sparkline data={[7]} />);

    expect(container.querySelector("polyline")).toBeNull();
  });

  it("renders a spacer when every point is a gap", () => {
    const data = [null, undefined, Number.NaN] as unknown as SparklineDatum[];
    const { container } = render(<Sparkline data={data} />);

    expect(container.querySelector("polyline")).toBeNull();
  });
});

describe("Sparkline -- gaps and null handling", () => {
  it("drops non-finite samples instead of plotting them as zero", () => {
    // A NaN plotted as 0 would drag the whole series to the floor and invent a
    // crash that never happened.
    const data = [10, Number.NaN, 30] as unknown as SparklineDatum[];
    const { container } = render(<Sparkline data={data} width={60} height={16} />);

    const pts = polylinePoints(container);
    expect(pts).toHaveLength(2);
    expect(pts[0]).toEqual([0, 15]); // min sits on the bottom of the margin box
    expect(pts[1]).toEqual([60, 1]); // max sits on the top
  });

  it("drops a null value inside an object series", () => {
    const data = [
      { value: 1 },
      { value: null },
      { value: 3 },
      { value: 2 },
    ] as unknown as SparklineDatum[];
    const { container } = render(<Sparkline data={data} />);

    expect(polylinePoints(container)).toHaveLength(3);
  });

  it("drops Infinity", () => {
    const data = [1, Infinity, 2, -Infinity] as unknown as SparklineDatum[];
    const { container } = render(<Sparkline data={data} />);

    expect(polylinePoints(container)).toHaveLength(2);
  });

  it("never emits NaN in the points attribute", () => {
    const data = [5, Number.NaN, 5, 5] as unknown as SparklineDatum[];
    const { container } = render(<Sparkline data={data} />);

    expect(container.querySelector("polyline")?.getAttribute("points")).not.toMatch(
      /NaN/,
    );
  });
});

describe("Sparkline -- geometry matches what recharts drew", () => {
  it("spreads samples evenly from x=0 to x=width", () => {
    const { container } = render(
      <Sparkline data={[0, 1, 2, 3, 4]} width={60} height={16} />,
    );

    expect(polylinePoints(container).map(([x]) => x)).toEqual([0, 15, 30, 45, 60]);
  });

  it("fills the margin box vertically, top 1 to height minus 1", () => {
    const { container } = render(
      <Sparkline data={[10, 20, 30]} width={60} height={16} />,
    );

    const ys = polylinePoints(container).map(([, y]) => y);
    expect(ys[0]).toBe(15);
    expect(ys[2]).toBe(1);
    expect(ys[1]).toBe(8); // midpoint of the box for the midpoint value
  });

  it("puts a flat series on the midline, as a zero-width domain did before", () => {
    const { container } = render(
      <Sparkline data={[5, 5, 5]} width={60} height={16} />,
    );

    expect(polylinePoints(container).map(([, y]) => y)).toEqual([8, 8, 8]);
  });

  it("honours a caller-supplied size", () => {
    const { container } = render(
      <Sparkline data={[1, 2]} width={120} height={40} />,
    );

    const svg = container.querySelector("svg");
    expect(svg).toHaveAttribute("width", "120");
    expect(svg).toHaveAttribute("height", "40");
    expect(svg).toHaveAttribute("viewBox", "0 0 120 40");
  });
});
