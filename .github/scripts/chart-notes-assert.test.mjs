// chart-notes-assert.test.mjs — the upgrade scenario's throwaway release
// handling in chart-notes-assert.mjs, against a fake `helm` that keeps a
// release record per release and refuses an upgrade of a release whose
// record is still pending, as Helm does.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import { notesForUpgrade } from './chart-notes-assert.mjs'

const NOTES = 'NOTES:\nupgrade notes\n'

// fakeHelm returns a helm runner whose n-th installed release reports the
// statuses records[n] on successive `helm status` reads (the last one
// sticks), and a log of the calls it served.
function fakeHelm(records) {
  const calls = []
  const releases = new Map()
  let installs = 0
  const helm = (args) => {
    calls.push(args[0])
    const [verb, name] = args
    if (verb === 'install') {
      releases.set(name, { reads: [...records[installs++]], status: records[installs - 1][0] })
      return ''
    }
    const rel = releases.get(name)
    if (verb === 'status') {
      if (rel.reads.length > 1) rel.status = rel.reads.shift()
      else rel.status = rel.reads[0]
      return JSON.stringify({ info: { status: rel.status } })
    }
    if (verb === 'upgrade') {
      if (rel.status !== 'deployed') throw new Error('Error: UPGRADE FAILED: another operation (install/upgrade/rollback) is in progress')
      return NOTES
    }
    if (verb === 'uninstall') return ''
    throw new Error(`unexpected helm ${verb}`)
  }
  return { helm, calls }
}

// clock is a fake time source sleep advances, so a bounded wait runs
// instantly.
function clock() {
  let t = 0
  return { now: () => t, sleep: (ms) => { t += ms } }
}

test('upgrades only once the throwaway release record reads deployed', () => {
  const { helm, calls } = fakeHelm([['pending-install', 'pending-install', 'deployed']])
  const notes = notesForUpgrade([], { helm, ...clock() })
  assert.equal(notes, NOTES)
  assert.deepEqual(calls, ['install', 'status', 'status', 'status', 'upgrade', 'uninstall'])
})

test('replaces a throwaway release whose record never settles', () => {
  const { helm, calls } = fakeHelm([['pending-install'], ['deployed']])
  const notes = notesForUpgrade([], { helm, ...clock(), timeoutMs: 3_000, pollMs: 1_000 })
  assert.equal(notes, NOTES)
  assert.equal(calls.filter((c) => c === 'install').length, 2)
  assert.equal(calls.filter((c) => c === 'uninstall').length, 2)
  assert.equal(calls.filter((c) => c === 'upgrade').length, 1)
})

test('fails naming the status once every throwaway release stays pending', () => {
  const { helm, calls } = fakeHelm([['pending-install'], ['pending-install']])
  assert.throws(
    () => notesForUpgrade([], { helm, ...clock(), timeoutMs: 3_000, pollMs: 1_000 }),
    /record still reads pending-install, not deployed.*attempt 2 of 2/,
  )
  assert.equal(calls.filter((c) => c === 'upgrade').length, 0)
  assert.equal(calls.filter((c) => c === 'uninstall').length, 2)
})
