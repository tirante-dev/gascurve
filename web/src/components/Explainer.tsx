"use client";

import type { LiveSnapshot, PricerModel } from "@/types";
import { formatDuration, formatGas, formatGasPerSecond, formatGwei } from "@/utils/format";
import { Prose } from "./primitives";

function ConstraintExplanation({ snapshot }: { snapshot: LiveSnapshot | null }) {
  const constraints = snapshot?.model === "constraints" ? snapshot.constraints : [];
  const shortest = constraints.length > 0 ? constraints.reduce((a, b) => (a.window < b.window ? a : b)) : null;
  const longest = constraints.length > 0 ? constraints.reduce((a, b) => (a.window > b.window ? a : b)) : null;
  const longCapacity = longest ? longest.target * longest.window : null;
  const shortCapacity = shortest ? shortest.target * shortest.window : null;

  return (
    <>
      <h3>Constraints are backlogs</h3>
      <p>
        Instead of one gas target, the chain keeps several constraints. Each has a target rate (compute gas per second), a window (seconds) and a backlog (gas). Every block, each backlog is first paid
        down at its target rate for the seconds that passed, then every transaction&apos;s compute gas is added to every backlog. The exponent is the sum over constraints of backlog divided by the product
        of target and window. The base fee is the minimum base fee multiplied by <code>P4(x)</code>
        {snapshot ? <>. The sampled minimum is {formatGwei(snapshot.minBaseFee)} gwei</> : null}.
      </p>

      {shortest && longest && longCapacity !== null && shortCapacity !== null && shortest !== longest ? (
        <>
          <h3>The {formatDuration(longest.window)} window is a ratchet</h3>
          <p>
            The {formatGasPerSecond(longest.target)} constraint has a capacity of {formatGas(longCapacity)} per unit of x. Whenever demand averages above {formatGasPerSecond(longest.target)}, the
            backlog grows for as long as that excess continues, and it takes sustained time below target to shed it. This is how average demand, rather than one short peak, can keep the fee elevated.
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
            A constraint with a long window reacts to average demand: sustained load above its target grows the backlog, and shedding it takes time below target. A constraint with a short window
            reacts to bursts and drains much sooner. Together they can produce a slow curve with sharp spikes on top.
          </p>
        </>
      )}

      <h3>The owner can reset the backlogs</h3>
      <p>
        A call to <code>setGasPricingConstraints</code> replaces the constraint set and sets each backlog to the starting value supplied with it. The fee can therefore jump, up or down, at an owner
        action. Those actions are marked on the history charts and listed under owner actions on the live page.
      </p>
    </>
  );
}

function LegacyExplanation({ snapshot }: { snapshot: LiveSnapshot | null }) {
  const legacy = snapshot?.model === "legacy" ? snapshot.legacy : undefined;
  const toleranceThreshold = legacy ? legacy.tolerance * legacy.speedLimit : null;
  const inertiaScale = legacy ? legacy.inertia * legacy.speedLimit : null;

  return (
    <>
      <h3>One backlog, one speed limit</h3>
      <p>
        The legacy pricer keeps one compute-gas backlog rather than a set of constraints. For each second that passes, it subtracts the speed limit from that backlog, stopping at zero. Each
        transaction&apos;s compute gas is added after the block is priced.
        {legacy ? <> The sampled speed limit is {formatGasPerSecond(legacy.speedLimit)}, and the current legacy backlog is {formatGas(legacy.backlog)}.</> : null}
      </p>

      <h3>Tolerance sets the floor region</h3>
      <p>
        Tolerance multiplied by the speed limit is the backlog threshold. At or below it, x is zero and the base fee stays at the minimum.
        {legacy && toleranceThreshold !== null ? <> The sampled tolerance is {legacy.tolerance}, which puts that threshold at {formatGas(toleranceThreshold)}.</> : null}
      </p>

      <h3>Inertia controls the response</h3>
      <p>
        Above the tolerance threshold, the pricer divides the excess backlog by inertia multiplied by the speed limit. Each full amount of that size adds 1 to x, then <code>P4(x)</code> multiplies
        the minimum base fee.
        {legacy && inertiaScale !== null ? <> The sampled inertia is {legacy.inertia}, so {formatGas(inertiaScale)} of excess backlog adds 1 to x.</> : null}
      </p>

      {!legacy ? <p>The current speed limit, inertia, tolerance and legacy backlog are unavailable right now.</p> : null}

      <h3>Legacy parameters can change</h3>
      <p>
        Owner actions can replace the speed limit, inertia or tolerance used for later blocks. The history charts mark those changes, and the live page lists the calls that made them.
      </p>
    </>
  );
}

function UnknownExplanation() {
  return (
    <>
      <h3>Pricing model unavailable</h3>
      <p>
        The api has not identified whether this network currently uses the constraint model or the legacy speed-limit model. Until it does, gascurve does not apply either model&apos;s parameters or
        backlog explanation to this network.
      </p>
    </>
  );
}

/** The mechanics in plain words, with live parameters only when the api supplied them. */
export function Explainer({ snapshot, model }: { snapshot: LiveSnapshot | null; model: PricerModel }) {
  return (
    <Prose>
      <h3>Three destinations, one fee</h3>
      <p>
        A transaction pays <code>gasUsed × baseFee</code>. Receipt <code>gasUsedForL1</code> is poster gas paid to the L1 pricer pool. The remaining compute gas is split between the infrastructure
        floor and network congestion fees. There is no priority fee: bidding more does not move a transaction forward.
      </p>

      {model === "constraints" ? <ConstraintExplanation snapshot={snapshot} /> : model === "legacy" ? <LegacyExplanation snapshot={snapshot} /> : <UnknownExplanation />}

      <h3>No tips, first come first served</h3>
      <p>
        The sequencer orders transactions as they arrive and does not collect tips. <code>eth_maxPriorityFeePerGas</code> is zero. Waiting for the pricing backlog to drain can lower the base fee.
      </p>

      <h3>P4 is a polynomial, not an exponential</h3>
      <p>
        Both pricing models supported here use <code>P4</code> after deriving x. It is the degree-4 Taylor expansion <code>1 + x + x²/2 + x³/6 + x⁴/24</code>, evaluated in integer basis points. Near zero it
        tracks e<sup>x</sup>; above x ≈ 2 it falls behind and grows like x⁴/24. An x that would mean a 25× fee under a true exponential gives about 20× here.
      </p>
    </Prose>
  );
}
