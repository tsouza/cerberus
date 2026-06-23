// Command resolve-release-trigger normalises the two entrypoints into
// `prepare-release.yml` — a manual `workflow_dispatch` and an `issues:
// labeled` event — into the three values the downstream `stage release files`
// step (prepare-release.mjs) consumes: `version`, `bump`, `chart_bump`.
//
// It writes those three keys to $GITHUB_OUTPUT (and echoes them) so the
// workflow can feed `steps.<id>.outputs.{version,bump,chart_bump}` into the
// staging step's env regardless of which trigger fired. prepare-release.mjs
// owns the semantics of the values (explicit VERSION overrides BUMP, BUMP=none
// is its no-op placeholder, CHART_BUMP defaults to patch); this resolver only
// decides which of the two event shapes supplied them.
//
// Env:
//   EVENT_NAME  GitHub event name: "workflow_dispatch" or "issues".
//
//   workflow_dispatch path (reads the three dispatch inputs verbatim):
//     VERSION     explicit target appVersion (e.g. "1.2.0"); may be empty
//     BUMP        none | patch | minor | major
//     CHART_BUMP  patch | minor | major (default patch when empty)
//
//   issues path (parses the just-applied label):
//     LABEL_NAME  the github.event.label.name, e.g. "release:minor" or
//                 "release:1.4.2". The contract:
//                   release:patch|minor|major  -> bump=<that>, chart_bump=patch
//                   release:<semver>           -> version=<semver>, chart_bump=patch
//                 anything else under `release:` is an error.
//
//   GITHUB_OUTPUT  runner file for step outputs (optional; echoed when absent).
//
// argv `--self-test` runs the in-process assertion suite and exits.
import process from 'node:process';
import { error, setOutput, log } from './lib/gh.mjs';

const LABEL_PREFIX = 'release:';
const LEVELS = new Set(['patch', 'minor', 'major']);
const SEMVER = /^v?\d+\.\d+\.\d+$/;
const DEFAULT_CHART_BUMP = 'patch';

// resolve() returns { version, bump, chart_bump } from a plain env object, or
// throws Error on bad input. Pure (no I/O) so the self-test can drive it.
export function resolve(env) {
  const eventName = (env.EVENT_NAME || '').trim();

  if (eventName === 'workflow_dispatch') {
    // Pass the dispatch inputs through untouched; prepare-release.mjs owns
    // the VERSION-overrides-BUMP and BUMP=none placeholder semantics.
    return {
      version: (env.VERSION || '').trim(),
      bump: (env.BUMP || '').trim(),
      chart_bump: (env.CHART_BUMP || '').trim() || DEFAULT_CHART_BUMP,
    };
  }

  if (eventName === 'issues') {
    const label = (env.LABEL_NAME || '').trim();
    if (!label.startsWith(LABEL_PREFIX)) {
      throw new Error(`label "${label}" is not a release: label`);
    }
    const arg = label.slice(LABEL_PREFIX.length).trim();
    if (LEVELS.has(arg)) {
      return { version: '', bump: arg, chart_bump: DEFAULT_CHART_BUMP };
    }
    if (SEMVER.test(arg)) {
      return { version: arg.replace(/^v/, ''), bump: '', chart_bump: DEFAULT_CHART_BUMP };
    }
    throw new Error(
      `unrecognised release label "${label}": expected ` +
        `release:patch|minor|major or release:<semver> (e.g. release:1.4.2)`,
    );
  }

  throw new Error(`unsupported EVENT_NAME "${eventName}" (want workflow_dispatch or issues)`);
}

function main() {
  let out;
  try {
    out = resolve(process.env);
  } catch (e) {
    error(`resolve-release-trigger: ${e.message}`);
    process.exit(1);
  }
  setOutput('version', out.version);
  setOutput('bump', out.bump);
  setOutput('chart_bump', out.chart_bump);
}

// --- self-test --------------------------------------------------------------

function selfTest() {
  const assert = (c, m) => {
    if (!c) throw new Error('self-test: ' + m);
  };
  const eq = (got, want, m) =>
    assert(JSON.stringify(got) === JSON.stringify(want), `${m}: got ${JSON.stringify(got)} want ${JSON.stringify(want)}`);
  const throws = (env, m) => {
    let threw = false;
    try {
      resolve(env);
    } catch {
      threw = true;
    }
    assert(threw, m);
  };

  // workflow_dispatch: inputs pass through; empty chart_bump defaults to patch.
  eq(
    resolve({ EVENT_NAME: 'workflow_dispatch', VERSION: '1.2.0', BUMP: 'none', CHART_BUMP: 'minor' }),
    { version: '1.2.0', bump: 'none', chart_bump: 'minor' },
    'dispatch explicit version',
  );
  eq(
    resolve({ EVENT_NAME: 'workflow_dispatch', VERSION: '', BUMP: 'minor', CHART_BUMP: '' }),
    { version: '', bump: 'minor', chart_bump: 'patch' },
    'dispatch bump, default chart_bump',
  );
  eq(
    resolve({ EVENT_NAME: 'workflow_dispatch' }),
    { version: '', bump: '', chart_bump: 'patch' },
    'dispatch all-empty (validation deferred to prepare-release.mjs)',
  );

  // issues: release:<level> -> bump=<level>, chart_bump=patch.
  for (const lvl of ['patch', 'minor', 'major']) {
    eq(
      resolve({ EVENT_NAME: 'issues', LABEL_NAME: `release:${lvl}` }),
      { version: '', bump: lvl, chart_bump: 'patch' },
      `issues release:${lvl}`,
    );
  }

  // issues: release:<semver> -> version=<semver> (v-prefix stripped), bump empty.
  eq(
    resolve({ EVENT_NAME: 'issues', LABEL_NAME: 'release:1.4.2' }),
    { version: '1.4.2', bump: '', chart_bump: 'patch' },
    'issues release:<semver>',
  );
  eq(
    resolve({ EVENT_NAME: 'issues', LABEL_NAME: 'release:v2.0.0' }),
    { version: '2.0.0', bump: '', chart_bump: 'patch' },
    'issues release:v<semver> strips v',
  );

  // issues: bad labels error.
  throws({ EVENT_NAME: 'issues', LABEL_NAME: 'release:lol' }, 'issues garbage suffix errors');
  throws({ EVENT_NAME: 'issues', LABEL_NAME: 'release:1.2' }, 'issues non-3-part semver errors');
  throws({ EVENT_NAME: 'issues', LABEL_NAME: 'bug' }, 'issues non-release label errors');
  throws({ EVENT_NAME: 'issues', LABEL_NAME: 'release:' }, 'issues bare release: errors');

  // unknown event name errors.
  throws({ EVENT_NAME: 'push' }, 'unknown event errors');
  throws({}, 'missing event errors');

  log('::notice::resolve-release-trigger --self-test: all assertions passed');
}

if (process.argv.includes('--self-test')) selfTest();
else main();
