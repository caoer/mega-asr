export const STATES = ["uploading", "uploaded", "processing", "aligned", "ingested", "failed", "deleted"] as const;
export type RecState = (typeof STATES)[number];

export interface FileRef {
  role: string;
  file?: string;
  codec?: string;
  bytes?: number;
  sha256?: string;
  verified?: boolean;
}

export interface Scores {
  rtf?: number;
  loops?: number;
  cer?: number;
  cpcer?: number;
  offset_s?: number;
  drift_ppm?: number;
  offset_score?: number;
  against?: string;
}

export interface Rec {
  id: string;
  title?: string;
  display_title?: string;
  labels?: string[];
  source?: string;
  host?: string;
  state: RecState | string;
  stage?: string;
  error?: string;
  started?: string;
  stopped?: string;
  duration_s?: number;
  speakers?: Speaker[];
  summary?: string | null;
  files?: FileRef[];
  scores?: Scores;
  wiki?: { page?: string; commit?: string };
  feishu?: { token?: string; transcript_file?: string };
  engine?: { root?: string };
  attempts?: number;
  owner?: string;
  /** The action a person queued (the drain clears it when it runs). */
  action?: ActionName;
  /** A drain's lease on the record: live while `expires` is in the future. */
  claim?: { by?: string; expires?: string };
  ingest?: Ingest;
  deleted_at?: string;
  deleted_by?: string;
  updated?: string;
}

/**
 * One diarised voice. `role` is the transcript's label at first naming (our
 * ASR's or Feishu's 「Speaker N」); `name` is who it is; `person` is the wiki
 * people slug once resolved. An unnamed entry carries only `name`.
 */
export interface Speaker {
  role?: string;
  name: string;
  person?: string;
}

/** The drain's ingest status, mirrored onto the record. */
export interface Ingest {
  run?: string;
  state?: string;
  step?: string;
  usd?: number;
  page_slug?: string;
  wiki_page?: string;
  error?: string;
  updated_at?: string;
}

/** The `labels` record (labels@1): the closed list and the agent's questions. */
export type LabelKindName = "type" | "project" | "person" | "topic";
export interface LabelsRecord {
  closed?: { name: string; kind: LabelKindName }[];
  questions?: LabelQuestion[];
}
/**
 * One question from the label agent (megameet labels ask). Status: open →
 * approved | declined | expired; approved → applied. `error` is the last
 * failed apply of an approved question.
 */
export interface LabelQuestion {
  id: string;
  asked?: string;
  text?: string;
  proposal?: string[];
  ask?: string;
  status?: string;
  answer?: string;
  answered?: string;
  applied?: string;
  error?: string;
  [k: string]: unknown;
}

/** One wiki page of a person: `{wiki, page, slug}`, `page` repo-relative. */
export interface PersonPage {
  wiki: string;
  page: string;
  slug: string;
}

/**
 * The `people.index` record (people_index@1), written by whatever maintains
 * the wikis: one entry per human. The entry's own fields are its primary page; `pages`
 * lists every page of that person, primary included.
 */
export interface PeopleIndexRecord {
  updated_at?: string;
  people: { slug: string; name: string; aliases?: string[]; wiki?: string; page?: string; pages?: PersonPage[]; summary?: string }[];
}

/**
 * The `config` record (meetings-config@1), put by the page owner: the
 * deployment's values, which the app does not ship. Optional; without it wiki
 * pages show unlinked and times render in the viewer's device zone.
 */
export interface PageConfig {
  /**
   * A wiki's web addresses, by the wiki name records and the people index use.
   * `page` is a template with `{path}` (the repo-relative page path); `commit`
   * adds `{commit}`, for a page as of an ingest commit.
   */
  wikis?: Record<string, { page?: string; commit?: string }>;
  /** The wiki a recording's `wiki.page` and its ingest's `wiki_page` live in. */
  ingest_wiki?: string;
  /** The zone (IANA name) a viewer sees until they choose one. */
  tz?: string;
}

export type ActionName = "retry" | "reingest" | "realign";

export const ACTIONS: Record<ActionName, { label: string; done: string; states: string[] }> = {
  retry: { label: "Retry", done: "Retry queued", states: ["failed"] },
  realign: { label: "Re-align", done: "Re-align queued", states: ["aligned", "ingested"] },
  reingest: { label: "Re-ingest", done: "Re-ingest queued", states: ["ingested"] },
};

export const AUDIO_ROLES = ["media", "mic", "remote", "beam", "raw"];
export const ROLE_LABEL: Record<string, string> = {
  media: "Media",
  mic: "Mic",
  remote: "Remote",
  beam: "Beam",
  raw: "Raw array",
};

export interface Segment {
  speaker: string;
  start: number;
  end: number;
  text: string;
  raw?: string | null;
}
