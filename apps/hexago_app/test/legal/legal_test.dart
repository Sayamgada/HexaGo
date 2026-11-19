import 'dart:convert';

import 'package:dio/dio.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/legal/legal_api.dart';
import 'package:hexago_app/features/legal/legal_panel.dart';

const _rules = {
  'max_surge_multiplier': 2.5,
  'min_insurance_coverage': 500000,
  'max_driving_hours': 12,
  'av_allowed_zones': ['876189260ffffff'],
  'required_documents': ['driving_licence', 'vehicle_rc'],
  'city_levy_pct': 2.0,
};

Map<String, dynamic> _jurisdiction({double cap = 2.5, int version = 1}) => {
      'city': 'bengaluru',
      'name': 'Bengaluru',
      'state': 'Karnataka',
      'authority': 'Karnataka Transport Department',
      'rules': {..._rules, 'max_surge_multiplier': cap},
      'version': version,
      'updated_at': version > 1 ? '2026-11-20T06:00:00Z' : null,
      'updated_by': version > 1 ? 'legal_jurisdiction' : 'seed',
    };

class _Adapter implements HttpClientAdapter {
  final requests = <String>[];
  Object? lastBody;

  @override
  Future<ResponseBody> fetch(RequestOptions o, Stream<List<int>>? body, Future<void>? cancel) async {
    requests.add('${o.method} ${o.uri.path}${o.uri.query.isEmpty ? '' : '?${o.uri.query}'}');
    lastBody = o.data;
    final p = o.uri.path;
    final Object json = p.endsWith('/v1/legal/jurisdictions')
        ? {'enabled': true, 'pricing_city': 'bengaluru', 'sample_notice': 'notice', 'jurisdictions': [_jurisdiction()]}
        : p.endsWith('/v1/legal/ingest')
            ? {
                'city': 'bengaluru', 'jurisdiction': _jurisdiction(cap: 1.5, version: 2), 'applied': true,
                'diff': [{'field': 'max_surge_multiplier', 'old': 2.5, 'new': 1.5}],
                'rejected': [{'field': 'city_levy_pct', 'value': 55, 'reason': 'outside 0..20'}],
                'overridden': [], 'audit_id': 'lga_1', 'source': 'llm', 'model': 'gemini/x', 'used_fallback': false,
                'summary': 'Surge capped at 1.5x.',
              }
            : {
                'entries': [
                  {'id': 'lga_1', 'city': 'bengaluru', 'created_at': '2026-11-20T06:00:00Z', 'source': 'llm',
                   'diff': [{'field': 'max_surge_multiplier', 'old': 2.5, 'new': 1.5}]},
                ],
              };
    return ResponseBody.fromString(jsonEncode(json), 200, headers: {
      Headers.contentTypeHeader: ['application/json'],
    });
  }

  @override
  void close({bool force = false}) {}
}

Future<void> _pump(WidgetTester tester, LegalApi api) async {
  tester.view.physicalSize = const Size(500, 1400);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
  await tester.pumpWidget(ProviderScope(
    overrides: [legalApiProvider.overrideWithValue(api)],
    child: const MaterialApp(home: Scaffold(body: LegalPanel())),
  ));
  await tester.pumpAndSettle();
}

void main() {
  group('formatting', () {
    test('Indian rupee grouping and rule values', () {
      expect(formatInr(500), '₹500');
      expect(formatInr(1500000), '₹15,00,000');
      expect(formatInr(20000000), '₹2,00,00,000');
      expect(formatInr(-123456), '-₹1,23,456');
      expect(formatRule('max_surge_multiplier', 1.5), '1.5x');
      expect(formatRule('max_surge_multiplier', 2), '2.0x');
      expect(formatRule('min_insurance_coverage', 500000), '₹5,00,000');
      expect(formatRule('max_driving_hours', 10.0), '10 h');
      expect(formatRule('city_levy_pct', 3.5), '3.5%');
      expect(formatRule('av_allowed_zones', <String>[]), 'none');
      expect(formatRule('required_documents', ['a', 'b']), 'a, b');
      expect(formatRule('other', 'x'), 'x');
      expect(formatRule('x', null), '—');
      expect(ruleLabel('city_levy_pct'), 'City levy');
      expect(ruleLabel('unknown'), 'unknown');
    });

    test('models parse the agents-py JSON', () {
      final l = JurisdictionList.fromJson({'jurisdictions': [_jurisdiction()], 'pricing_city': 'bengaluru'});
      final j = l.byCity('bengaluru')!;
      expect(j.rules.maxSurge, 2.5);
      expect(j.rules.minInsurance, 500000);
      expect(j.rules.documents, ['driving_licence', 'vehicle_rc']);
      expect(j.rules.toJson()['max_driving_hours'], 12);
      expect(l.byCity('paris'), isNull);
      expect(l.sampleNotice, defaultSampleNotice);
      final r = IngestResult.fromJson({'city': 'bengaluru', 'jurisdiction': _jurisdiction(), 'applied': false});
      expect(r.diff, isEmpty);
      expect(r.applied, isFalse);
    });
  });

  test('Dio API hits the gateway legal endpoints', () async {
    final adapter = _Adapter();
    final api = DioLegalApi(apiBaseUrl: 'http://gw/', dio: Dio()..httpClientAdapter = adapter);
    expect((await api.jurisdictions()).sampleNotice, 'notice');
    final r = await api.ingest('surge cap 1.5x', city: 'bengaluru');
    expect(r.diff.single.newValue, 1.5);
    expect(adapter.lastBody, {'text': 'surge cap 1.5x', 'city': 'bengaluru'});
    expect((await api.audit(limit: 3)).single.id, 'lga_1');
    expect(adapter.requests, ['GET /v1/legal/jurisdictions', 'POST /v1/legal/ingest', 'GET /v1/legal/audit?limit=3']);
  });

  test('mock API applies a surge cap and audits', () async {
    final api = MockLegalApi();
    final r = await api.ingest(defaultSampleNotice);
    expect(r.city, 'bengaluru');
    expect(r.applied, isTrue);
    expect(r.jurisdiction.rules.maxSurge, 1.5);
    final again = await api.ingest('Maharashtra notice: no change here');
    expect(again.city, 'mumbai');
    expect(again.applied, isFalse);
    expect((await api.audit()).length, 2);
    expect((await api.ingest('Delhi notice')).city, 'delhi');
  });

  testWidgets('Legal tab: rules, pre-filled sample, ingest shows the diff and the new cap', (tester) async {
    await _pump(tester, MockLegalApi());
    expect(find.text('Legal & Jurisdiction'), findsOneWidget);
    expect(find.byKey(const ValueKey('legal-rules-bengaluru')), findsOneWidget);
    expect(find.text('PRICING CITY'), findsOneWidget);
    final cap = find.byKey(const ValueKey('legal-cap-bengaluru'));
    expect(tester.widget<Text>(cap).data, '2.5x');
    final field = tester.widget<TextField>(find.byKey(const ValueKey('legal-text')));
    expect(field.controller!.text, contains('capped at 1.5x the base fare'));

    await tester.tap(find.byKey(const ValueKey('legal-ingest')));
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('legal-result')), findsOneWidget);
    expect(find.text('APPLIED'), findsOneWidget);
    expect(find.byKey(const ValueKey('legal-diff-max_surge_multiplier')), findsOneWidget);
    expect(tester.widget<Text>(find.byKey(const ValueKey('legal-cap-bengaluru'))).data, '1.5x');
    expect(find.textContaining('Audit log'), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('legal-city-mumbai')));
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('legal-rules-mumbai')), findsOneWidget);
  });

  testWidgets('Legal tab shows rejected values and the LLM badge', (tester) async {
    await _pump(tester, DioLegalApi(apiBaseUrl: 'http://gw', dio: Dio()..httpClientAdapter = _Adapter()));
    await tester.tap(find.byKey(const ValueKey('legal-ingest')));
    await tester.pumpAndSettle();
    expect(find.text('LLM'), findsOneWidget);
    expect(find.textContaining('Rejected City levy: 55'), findsOneWidget);
    expect(find.text('Surge capped at 1.5x.'), findsOneWidget);
  });

  testWidgets('Legal tab reports an unreachable service', (tester) async {
    await _pump(tester, _FailingApi());
    expect(find.textContaining('jurisdiction rules'), findsOneWidget);
  });
}

class _FailingApi implements LegalApi {
  @override
  Future<List<AuditEntry>> audit({int limit = 10}) => throw StateError('down');

  @override
  Future<IngestResult> ingest(String text, {String? city}) => throw StateError('down');

  @override
  Future<JurisdictionList> jurisdictions() => throw StateError('down');
}
