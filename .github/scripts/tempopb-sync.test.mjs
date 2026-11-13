// Self-test for tempopb-sync.mjs's file selection and source rewrites.
import { test } from 'node:test';
import assert from 'node:assert/strict';

import { isCopied, rewriteGo, upstreamCommit } from './tempopb-sync.mjs';

test('the pin file names one 40-hex upstream commit', () => {
  const sha = '2f74ea818de1377e6ac9dd15117fa284c9c6ba36';
  assert.equal(upstreamCommit(`# comment\nupstream_repo: x\nupstream_commit:  ${sha}\n`), sha);
  assert.throws(() => upstreamCommit('upstream_commit: 2f74ea8\n'), /upstream_commit/);
  assert.throws(() => upstreamCommit('upstream_repo: x\n'), /upstream_commit/);
});

test('only the generated wire types, their protos and the subtree licence are copied', () => {
  for (const rel of ['tempo.pb.go', 'trace/v1/trace.pb.go', 'tempo.proto', 'LICENSE_APACHE2']) {
    assert.equal(isCopied(rel), true, rel);
  }
  for (const rel of ['pool.go', 'prealloc.go', 'utils.go', 'trace_utils.go', 'utils_test.go', 'trace.json']) {
    assert.equal(isCopied(rel), false, rel);
  }
});

test('imports move to the in-house path and registry names to the cerberus namespace', () => {
  const upstream = [
    'import (',
    '\tv1 "github.com/grafana/tempo/pkg/tempopb/common/v1"',
    ')',
    '\tKind Span_SpanKind `protobuf:"varint,6,opt,name=kind,proto3,enum=tempopb.trace.v1.Span_SpanKind" json:"kind,omitempty"`',
    '\tproto.RegisterEnum("tempopb.trace.v1.Span_SpanKind", Span_SpanKind_name, Span_SpanKind_value)',
    '\tproto.RegisterType((*Span)(nil), "tempopb.trace.v1.Span")',
    '\tproto.RegisterMapType((map[string]string)(nil), "tempopb.SearchRequest.TagsEntry")',
    '\tServiceName: "tempopb.StreamingQuerier",',
    '\terr := c.cc.Invoke(ctx, "/tempopb.Querier/FindTraceByID", in, out, opts...)',
  ].join('\n');
  const want = [
    'import (',
    '\tv1 "github.com/tsouza/cerberus/internal/thirdparty/tempopb/common/v1"',
    ')',
    '\tKind Span_SpanKind `protobuf:"varint,6,opt,name=kind,proto3,enum=cerberus.tempopb.trace.v1.Span_SpanKind" json:"kind,omitempty"`',
    '\tproto.RegisterEnum("cerberus.tempopb.trace.v1.Span_SpanKind", Span_SpanKind_name, Span_SpanKind_value)',
    '\tproto.RegisterType((*Span)(nil), "cerberus.tempopb.trace.v1.Span")',
    '\tproto.RegisterMapType((map[string]string)(nil), "cerberus.tempopb.SearchRequest.TagsEntry")',
    // The gRPC wire names are the protocol and stay upstream's.
    '\tServiceName: "tempopb.StreamingQuerier",',
    '\terr := c.cc.Invoke(ctx, "/tempopb.Querier/FindTraceByID", in, out, opts...)',
  ].join('\n');
  assert.equal(rewriteGo(upstream), want);
});
