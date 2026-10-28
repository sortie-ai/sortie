import { Code } from "@/components/Code";
import { Pair, Pairs, Panel } from "@/components/Panel";
import { Hint } from "@/components/ui/hint";
import { fmtCount, fmtStamp, plural } from "@/lib/format";
import { layerLabel } from "@/lib/rates";

import type {
  TableInForceResponse,
  TableSourceResponse,
  TokenRatesSourceResponse,
} from "@/api/types.gen";
import type { ReactNode } from "react";

const DIGEST_PREFIX = "sha256:";
const SHORT_LENGTH = 12;

const STATUS_WORDS: Readonly<Record<string, string>> = {
  not_loaded: "Not loaded yet",
  loaded: "Loaded",
  failing: "Failing",
};

interface StampProps {
  at: string | null;
}

function Stamp({ at }: StampProps) {
  return at === null ? undefined : fmtStamp(at);
}

interface ShortValueProps {
  value: string | null;
  /** Dropped from the front of the value before it is cut. */
  prefix?: string;
}

function ShortValue({ value, prefix = "" }: ShortValueProps) {
  if (value === null) {
    return undefined;
  }
  const bare = value.startsWith(prefix) ? value.slice(prefix.length) : value;
  return <Hint hint={value}>{bare.slice(0, SHORT_LENGTH)}</Hint>;
}

interface StatusValueProps {
  source: TableSourceResponse;
  /** The word of a loaded table; the embedded table reads "On". */
  loaded?: string;
}

function StatusValue({ source, loaded = STATUS_WORDS.loaded }: StatusValueProps): ReactNode {
  if (source.status === "off") {
    return source.off_by === null ? (
      "Off"
    ) : (
      <>
        Off by <Code>{source.off_by}</Code>
      </>
    );
  }
  return source.status === "loaded" ? loaded : (STATUS_WORDS[source.status] ?? source.status);
}

interface SourceProps {
  source: TableSourceResponse;
}

function InForcePair({ source }: SourceProps) {
  return <Pair label="In force">{source.in_force ? "Yes" : "No"}</Pair>;
}

function ModelsPair({ source }: SourceProps) {
  return <Pair label="Models">{source.models === null ? undefined : fmtCount(source.models)}</Pair>;
}

function DigestPair({ source }: SourceProps) {
  return (
    <Pair label="Version">
      <ShortValue value={source.version} prefix={DIGEST_PREFIX} />
    </Pair>
  );
}

function FailureRows({ source }: SourceProps) {
  if (source.status !== "failing") {
    return undefined;
  }
  return (
    <>
      {source.failures > 0 ? (
        <Pair label="Failures in a row">{fmtCount(source.failures)}</Pair>
      ) : undefined}
      <Pair label="Last failure">
        <Stamp at={source.failed_at} />
      </Pair>
      <Pair label="Error">{source.error}</Pair>
    </>
  );
}

export interface PricesInForcePanelProps {
  table: TableInForceResponse | null;
}

export function PricesInForcePanel({ table }: PricesInForcePanelProps) {
  return (
    <Panel title="Prices in force">
      <Pairs>
        <Pair label="Source">{table === null ? "None" : layerLabel(table.layer)}</Pair>
        <Pair label="Prices as of">{table === null ? undefined : fmtStamp(table.as_of)}</Pair>
        <Pair label="Age">
          {table === null ? undefined : plural(table.age_days, "day", "days")}
        </Pair>
      </Pairs>
    </Panel>
  );
}

function entriesText(source: TokenRatesSourceResponse): string {
  const parts = [
    source.kind_entries > 0 ? plural(source.kind_entries, "kind entry", "kind entries") : "",
    source.model_entries > 0 ? plural(source.model_entries, "model entry", "model entries") : "",
  ].filter((part) => part !== "");
  return parts.length === 0 ? "Not set" : parts.join(", ");
}

export interface TokenRatesPanelProps {
  source: TokenRatesSourceResponse;
}

export function TokenRatesPanel({ source }: TokenRatesPanelProps) {
  return (
    <Panel title="Token rates">
      <Pairs>
        <Pair label="Entries">{entriesText(source)}</Pair>
        <Pair label="Version">
          <ShortValue value={source.version} prefix={DIGEST_PREFIX} />
        </Pair>
      </Pairs>
    </Panel>
  );
}

export function OperatorTablePanel({ source }: SourceProps) {
  const isFile = source.file !== null;
  return (
    <Panel title="Custom prices">
      <Pairs>
        <Pair label="Status">
          <StatusValue source={source} />
        </Pair>
        <InForcePair source={source} />
        <Pair label={isFile ? "Loaded" : "Last download"}>
          <Stamp at={source.as_of} />
        </Pair>
        {isFile ? undefined : (
          <Pair label="Next download">
            <Stamp at={source.next_try_at} />
          </Pair>
        )}
        <DigestPair source={source} />
        <ModelsPair source={source} />
        <FailureRows source={source} />
      </Pairs>
    </Panel>
  );
}

export function DownloadedTablePanel({ source }: SourceProps) {
  return (
    <Panel title="Downloaded prices">
      <Pairs>
        <Pair label="Status">
          <StatusValue source={source} />
        </Pair>
        <InForcePair source={source} />
        <Pair label="Last download">
          <Stamp at={source.as_of} />
        </Pair>
        <Pair label="Next download">
          <Stamp at={source.next_try_at} />
        </Pair>
        <DigestPair source={source} />
        <ModelsPair source={source} />
        <FailureRows source={source} />
      </Pairs>
    </Panel>
  );
}

export function EmbeddedTablePanel({ source }: SourceProps) {
  return (
    <Panel title="Built-in prices">
      <Pairs>
        <Pair label="Status">
          <StatusValue source={source} loaded="On" />
        </Pair>
        <InForcePair source={source} />
        <Pair label="Version">
          <ShortValue value={source.version} />
        </Pair>
        <Pair label="Version date">
          <Stamp at={source.as_of} />
        </Pair>
        <ModelsPair source={source} />
      </Pairs>
    </Panel>
  );
}
