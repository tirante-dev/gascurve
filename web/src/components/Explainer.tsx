"use client";

import type { LiveSnapshot } from "@/types";
import { formatDuration, formatGas, formatGasPerSecond, formatGwei } from "@/utils/format";
import { Prose } from "./primitives";

/** The mechanics in plain words, with the live parameters filled in where they matter. */
export function Explainer({ snapshot }: { snapshot: LiveSnapshot | null }) {
  const constraints = snapshot?.constraints ?? [];
  const shortest = constraints.length > 0 ? constraints.reduce((a, b) => (a.window < b.window ? a : b)) : null;
  const longest = constraints.length > 0 ? constraints.reduce((a, b) => (a.window > b.window ? a : b)) : null;
  const floor = snapshot ? formatGwei(snapshot.minBaseFee) : "0.02";
  const longCapacity = longest ? longest.target * longest.window : 3_456_000_000_000;
  const shortCapacity = shortest ? shortest.target * shortest.window : 900_000_000;
  return (
    <Prose>
      <h3>Three destinations, one fee</h3>
      <p>
        A transaction pays <code>gasUsed × baseFee</code>. Receipt <code>gasUsedForL1</code> is poster gas paid to the L1 pricer pool. The remaining compute gas is split between the infrastructure
        floor and network congestion fees. There is no priority fee: bidding more does not move a transaction forward.
      </p>

      <h3>Constraints are backlogs</h3>
      <p>
        Instead of one gas target, the chain keeps several constraints. Each has a target rate (compute gas per second), a window (seconds) and a backlog (gas). Every block, each backlog is first paid
        down at its target rate for the seconds that passed, then every transaction&apos;s compute gas is added to every backlog. The exponent is the sum over constraints of backlog divided by target
        times window, and the base fee is the floor ({floor} gwei) multiplied by <code>P4(x)</code>.
      </p>

      {constraints.length > 0 && shortest && longest && shortest !== longest ? (
        <>
          <h3>The {formatDuration(longest.window)} window is a ratchet</h3>
          <p>
            The {formatGasPerSecond(longest.target)} constraint has a capacity of {formatGas(longCapacity)} per unit of x. Whenever demand averages above {formatGasPerSecond(longest.target)}
            the backlog grows all day long, and it takes a full day below target to shed one day of excess. That is why the fee can sit at the floor for weeks and then climb for eleven days
            straight: the average, not the peak, is what moves it.
          </p>

          <h3>The {formatDuration(shortest.window)} window is the spike engine</h3>
          <p>
            The {formatGasPerSecond(shortest.target)} constraint reaches x = 1 after only {formatGas(shortCapacity)} of backlog. A burst of a few blocks fills it, and it drains at{" "}
            {formatGasPerSecond(shortest.target)}, so it is usually back to zero within seconds of demand dropping. It produces the short spikes on top of the slow curve.
          </p>
        </>
      ) : (
        <>
          <h3>Long windows ratchet, short windows spike</h3>
          <p>
            A constraint with a day-long window reacts to the average demand: sustained load above its target grows the backlog for hours, and shedding it takes just as long. A constraint
            with a window of seconds reacts to bursts and drains almost immediately. Together they give a slow curve with sharp spikes on top.
          </p>
        </>
      )}

      <h3>No tips, first come first served</h3>
      <p>
        The sequencer orders transactions as they arrive and does not collect tips. <code>eth_maxPriorityFeePerGas</code> is zero. The only way to pay less is to wait for the backlogs to
        drain.
      </p>

      <h3>The owner can reset the backlogs</h3>
      <p>
        A call to <code>setGasPricingConstraints</code> replaces the constraint set and sets each backlog to the starting value supplied with it. The fee can therefore jump, up or down, at an
        owner action. Those actions are marked on the history charts and listed under owner actions on the live page.
      </p>

      <h3>P4 is a polynomial, not an exponential</h3>
      <p>
        <code>P4</code> is the degree-4 Taylor expansion <code>1 + x + x²/2 + x³/6 + x⁴/24</code>, evaluated in integer basis points. Near zero it tracks e<sup>x</sup>; above x ≈ 2 it falls
        behind and grows like x⁴/24. A backlog that would mean a 25× fee under a true exponential gives about 20× here.
      </p>
    </Prose>
  );
}
