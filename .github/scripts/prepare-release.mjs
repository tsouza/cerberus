// Command prepare-release stages a cerberus release: it bumps the chart
// `version` + `appVersion`, refreshes the Artifact Hub `changes` annotation,
// rewrites the CHANGELOG `[Unreleased]` section into a dated release section,
// and writes a PR body — all derived from the conventional commits since the
// last `v*` tag. The `prepare-release.yml` workflow runs it, regenerates the
// chart README via helm-docs, then opens the PR.
//
// Env:
//   VERSION     explicit target appVersion (e.g. "1.2.0"); overrides BUMP
//   BUMP        patch | minor | major — used when VERSION is empty
//   CHART_BUMP  patch | minor | major — chart `version:` bump (default patch)
//   GITHUB_OUTPUT  runner file for step outputs (optional)
//   PR_BODY_FILE   path to write the generated PR body (default release-pr-body.md)
//
// argv `--self-test` runs the in-process assertion suite and exits.
import { readFileSync, writeFileSync, appendFileSync } from 'node:fs'
import { execFileSync } from 'node:child_process'

const CHART = 'deploy/helm/cerberus/Chart.yaml'
const CHANGELOG = 'CHANGELOG.md'
const IMAGE = 'ghcr.io/tsouza/cerberus'

// --- semver -----------------------------------------------------------------

export function bumpSemver(version, level) {
  const m = /^v?(\d+)\.(\d+)\.(\d+)$/.exec(version.trim())
  if (!m) throw new Error(`not a 3-part semver: "${version}"`)
  let [maj, min, pat] = m.slice(1).map(Number)
  if (level === 'major') return `${maj + 1}.0.0`
  if (level === 'minor') return `${maj}.${min + 1}.0`
  if (level === 'patch') return `${maj}.${min}.${pat + 1}`
  throw new Error(`unknown bump level: "${level}"`)
}

// --- conventional-commit grouping -------------------------------------------

// type -> changelog section heading (order matters; unlisted types are dropped
// from the user-facing changelog but still listed in the PR body's "Other").
const SECTIONS = [
  ['feat', 'Added'],
  ['fix', 'Fixed'],
  ['perf', 'Performance'],
  ['refactor', 'Changed'],
  ['ci', 'CI'],
  ['docs', 'Documentation'],
]
// type -> Artifact Hub change kind (only these types seed annotations).
const AH_KIND = { feat: 'added', fix: 'fixed', perf: 'changed', refactor: 'changed' }

export function parseCommits(subjects) {
  const groups = {}
  const breaking = []
  for (const raw of subjects) {
    const s = raw.trim()
    if (!s) continue
    const m = /^(\w+)(?:\(([^)]+)\))?(!)?:\s*(.+)$/.exec(s)
    if (!m) continue
    const [, type, scope, bang, descRaw] = m
    const desc = descRaw.trim()
    const entry = { type, scope: scope || '', desc, breaking: !!bang }
    ;(groups[type] ||= []).push(entry)
    if (bang) breaking.push(entry)
  }
  return { groups, breaking }
}

function bullets(entries) {
  return entries.map((e) => `- ${e.scope ? `**${e.scope}:** ` : ''}${e.desc}`).join('\n')
}

// Renders only the `### ...` subsections for a release; the caller owns the
// `## [vX] — date` heading so there is exactly one.
export function renderChangelogSection(parsed) {
  const out = []
  if (parsed.breaking.length) {
    out.push('### BREAKING', '', bullets(parsed.breaking), '')
  }
  for (const [type, heading] of SECTIONS) {
    const entries = parsed.groups[type]
    if (entries && entries.length) out.push(`### ${heading}`, '', bullets(entries), '')
  }
  // Trim trailing blank line so we don't accumulate them on insert.
  while (out.length && out[out.length - 1] === '') out.pop()
  return out.join('\n')
}

// yamlDq escapes a single-line string for a YAML double-quoted scalar:
// backslash FIRST (so we don't double-escape the escapes we add), then the
// quote char. Complete escaping — partial schemes (e.g. quote-only) are an
// injection vector into the rendered YAML.
export function yamlDq(s) {
  return s.replace(/\\/g, '\\\\').replace(/"/g, '\\"')
}

export function renderAhChanges(parsed) {
  const lines = []
  for (const [type, kind] of Object.entries(AH_KIND)) {
    for (const e of parsed.groups[type] || []) {
      const text = yamlDq(`${e.scope ? `${e.scope}: ` : ''}${e.desc}`)
      lines.push(`    - kind: ${kind}`)
      lines.push(`      description: "${text}"`)
    }
  }
  // Artifact Hub caps the list usefully; keep the top ~10 entries.
  return lines.slice(0, 20).join('\n')
}

// --- Chart.yaml + CHANGELOG edits -------------------------------------------

export function editChart(text, { chartVersion, appVersion, ahChanges }) {
  let out = text
  out = out.replace(/^version:\s.*$/m, `version: ${chartVersion}`)
  out = out.replace(/^appVersion:\s.*$/m, `appVersion: "${appVersion}"`)
  out = out.replace(
    new RegExp(`(image:\\s*${IMAGE.replace(/[.]/g, '\\.')}:)v[\\w.\\-]+`),
    `$1v${appVersion}`,
  )
  // Replace the whole `artifacthub.io/changes: |` block scalar (it is the last
  // key in the file, so it runs to EOF).
  out = out.replace(
    /( {2}artifacthub\.io\/changes:\s*\|\n)([\s\S]*)$/,
    (_all, head) => `${head}${ahChanges}\n`,
  )
  return out
}

export function editChangelog(text, { version, date, section }) {
  const marker = '## [Unreleased]'
  const i = text.indexOf(marker)
  if (i === -1) throw new Error(`no "${marker}" section in ${CHANGELOG}`)
  const after = i + marker.length
  // Capture any hand-written entries currently under [Unreleased] up to the
  // next "## [" heading; fold them into the release section above the
  // generated one, then leave a fresh empty [Unreleased].
  const rest = text.slice(after)
  const next = rest.search(/\n## \[/)
  const carried = (next === -1 ? rest : rest.slice(0, next)).trim()
  const tail = next === -1 ? '' : rest.slice(next)
  // Prefer hand-curated [Unreleased] entries (Keep a Changelog flow); fall back
  // to the commit-derived section only when [Unreleased] was left empty.
  const body = carried || section
  return `${text.slice(0, after)}\n\n## [v${version}] — ${date}\n\n${body}\n${tail}`
}

// --- driver -----------------------------------------------------------------

function git(args) {
  return execFileSync('git', args, { encoding: 'utf8' })
}

function commitsSinceLastTag() {
  let range = 'HEAD'
  try {
    const last = git(['describe', '--tags', '--abbrev=0', '--match', 'v*']).trim()
    if (last) range = `${last}..HEAD`
  } catch {
    // no prior tag — use whole history
  }
  return git(['log', range, '--no-merges', '--format=%s'])
    .split('\n')
    .filter(Boolean)
}

function today() {
  return new Date().toISOString().slice(0, 10)
}

function setOutput(k, v) {
  if (process.env.GITHUB_OUTPUT) appendFileSync(process.env.GITHUB_OUTPUT, `${k}=${v}\n`)
  console.log(`${k}=${v}`)
}

function main() {
  const chartText = readFileSync(CHART, 'utf8')
  const curChart = /^version:\s*(.+)$/m.exec(chartText)[1].trim()
  const curApp = /^appVersion:\s*"?([^"\n]+)"?$/m.exec(chartText)[1].trim()

  const explicit = (process.env.VERSION || '').trim()
  let bump = (process.env.BUMP || '').trim()
  if (bump === 'none') bump = '' // the workflow's no-op placeholder
  if (!explicit && !bump) throw new Error('one of VERSION or BUMP is required')
  const appVersion = explicit ? explicit.replace(/^v/, '') : bumpSemver(curApp, bump)
  if (!/^\d+\.\d+\.\d+$/.test(appVersion)) throw new Error(`bad target appVersion: "${appVersion}"`)
  const chartVersion = bumpSemver(curChart, (process.env.CHART_BUMP || 'patch').trim())

  const parsed = parseCommits(commitsSinceLastTag())
  const date = today()
  const section = renderChangelogSection(parsed)
  const ahChanges = renderAhChanges(parsed) || '    - kind: changed\n      description: "release v' + appVersion + '"'

  writeFileSync(CHART, editChart(chartText, { chartVersion, appVersion, ahChanges }))
  writeFileSync(CHANGELOG, editChangelog(readFileSync(CHANGELOG, 'utf8'), { version: appVersion, date, section }))

  const prBody = [
    `Release-prep for **v${appVersion}** (chart **${chartVersion}**), generated by \`prepare-release.yml\`.`,
    '',
    `- appVersion ${curApp} -> ${appVersion}, chart ${curChart} -> ${chartVersion}`,
    '',
    '### Changes',
    '',
    section,
    '',
    'Generated from the conventional commits since the last tag. Review and edit before merging; the tag is cut once this lands.',
  ].join('\n')
  writeFileSync(process.env.PR_BODY_FILE || 'release-pr-body.md', prBody + '\n')

  setOutput('new_version', appVersion)
  setOutput('chart_version', chartVersion)
}

// --- self-test --------------------------------------------------------------

function selfTest() {
  const assert = (c, m) => { if (!c) throw new Error('self-test: ' + m) }
  assert(bumpSemver('1.0.2', 'patch') === '1.0.3', 'patch')
  assert(bumpSemver('1.0.2', 'minor') === '1.1.0', 'minor')
  assert(bumpSemver('v1.2.3', 'major') === '2.0.0', 'major (v-prefix)')
  let threw = false
  try { bumpSemver('1.2', 'patch') } catch { threw = true }
  assert(threw, 'rejects non-3-part')

  const p = parseCommits([
    'feat(promql): native grid (#1)',
    'fix: leak (#2)',
    'perf(solver): cow',
    'chore: noise',
    'refactor(api)!: drop legacy field',
  ])
  assert(p.groups.feat.length === 1 && p.groups.feat[0].scope === 'promql', 'feat parsed')
  assert(p.groups.chore.length === 1, 'chore captured but not rendered')
  assert(p.breaking.length === 1 && p.breaking[0].type === 'refactor', 'breaking flagged')

  const sec = renderChangelogSection(p)
  assert(!sec.includes('## [v'), 'section omits the version header')
  assert(sec.includes('### BREAKING'), 'breaking section')
  assert(sec.includes('### Added') && sec.includes('native grid'), 'added section')
  assert(!sec.includes('noise'), 'chore excluded from changelog')

  const ah = renderAhChanges(p)
  assert(ah.includes('kind: added') && ah.includes('kind: fixed'), 'ah kinds')
  assert(!ah.includes('kind: chore'), 'ah excludes chore')
  assert(yamlDq('say "hi"\\n') === 'say \\"hi\\"\\\\n', 'yamlDq escapes quote + backslash')
  const ahQuoted = renderAhChanges(parseCommits(['feat: add "fast" path \\ here']))
  assert(ahQuoted.includes('add \\"fast\\" path \\\\ here'), 'ah description fully escaped')

  const chart = [
    'version: 0.3.2',
    'appVersion: "1.0.2"',
    '  artifacthub.io/images: |',
    '    - name: cerberus',
    '      image: ghcr.io/tsouza/cerberus:v1.0.2',
    '  artifacthub.io/changes: |',
    '    - kind: changed',
    '      description: "old"',
  ].join('\n') + '\n'
  const edited = editChart(chart, { chartVersion: '0.4.0', appVersion: '1.1.0', ahChanges: ah })
  assert(/^version: 0\.4\.0$/m.test(edited), 'chart version bumped')
  assert(/^appVersion: "1\.1\.0"$/m.test(edited), 'appVersion bumped')
  assert(edited.includes('cerberus:v1.1.0'), 'image tag bumped')
  assert(edited.includes('kind: added') && !edited.includes('"old"'), 'ah block replaced')

  const cl = '# Changelog\n\n## [Unreleased]\n\n### Added\n\n- carried entry\n\n## [v1.0.0] — 2026-06-17\n\nfirst\n'
  const ecl = editChangelog(cl, { version: '1.1.0', date: '2026-06-19', section: sec })
  assert(ecl.indexOf('## [Unreleased]') < ecl.indexOf('## [v1.1.0]'), 'fresh unreleased on top')
  assert(ecl.includes('carried entry'), 'carried entries preserved')
  assert(!ecl.includes('### BREAKING'), 'generated section not duplicated when curated entries exist')
  assert(ecl.indexOf('## [v1.1.0]') < ecl.indexOf('## [v1.0.0]'), 'new release above old')
  assert((ecl.match(/## \[v1\.1\.0\]/g) || []).length === 1, 'single v1.1.0 header')

  console.log('::notice::prepare-release --self-test: all assertions passed')
}

if (process.argv.includes('--self-test')) selfTest()
else main()
