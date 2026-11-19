import 'dart:convert';

import 'package:dio/dio.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/report/report_panel.dart';
import 'package:hexago_app/widgets/simple_markdown.dart';

const _md = '# Autonomous Ops Report\n'
    '_Generated 2026-11-20 06:00 UTC · trigger: manual_\n\n'
    '## Executive summary\nThe swarm made **1,284** decisions.\n\n'
    '| KPI | Value |\n|---|---|\n| Matches today | 540 |\n\n'
    '## Incidents and actions\n- 2 incidents\n  - `inc_1` escalated\n\n'
    '## Human interventions: 0\n';

class _Adapter implements HttpClientAdapter {
  final requests = <String>[];

  @override
  Future<ResponseBody> fetch(RequestOptions o, Stream<List<int>>? body, Future<void>? cancel) async {
    requests.add('${o.method} ${o.uri.path}');
    return ResponseBody.fromString(
        jsonEncode({
          'id': o.method == 'POST' ? 'opr_2' : 'opr_1', 'generated_at': '2026-11-20T06:00:00Z',
          'trigger': o.method == 'POST' ? 'manual' : 'schedule', 'anomalies': [{}], 'summary_source': 'llm',
          'markdown': _md,
        }),
        200,
        headers: {Headers.contentTypeHeader: ['application/json']});
  }

  @override
  void close({bool force = false}) {}
}

class _FailingApi implements ReportApi {
  @override
  Future<OpsReport> generate() => throw StateError('down');

  @override
  Future<OpsReport> latest() => throw StateError('down');
}

Future<void> _pump(WidgetTester tester, ReportApi api) async {
  tester.view.physicalSize = const Size(500, 1200);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
  await tester.pumpWidget(ProviderScope(
    overrides: [reportApiProvider.overrideWithValue(api)],
    child: const MaterialApp(home: Scaffold(body: ReportPanel())),
  ));
  await tester.pumpAndSettle();
}

void main() {
  test('Dio API reads the latest report and generates one', () async {
    final adapter = _Adapter();
    final api = DioReportApi(apiBaseUrl: 'http://gw/', dio: Dio()..httpClientAdapter = adapter);
    final latest = await api.latest();
    expect(latest.id, 'opr_1');
    expect(latest.anomalies, 1);
    expect(latest.summarySource, 'llm');
    expect((await api.generate()).trigger, 'manual');
    expect(adapter.requests, ['GET /v1/ops/report', 'POST /v1/ops/report/run']);
  });

  testWidgets('Report tab renders the markdown with its timestamp and generates on demand', (tester) async {
    await _pump(tester, MockReportApi(now: () => DateTime.utc(2026, 11, 20, 6, 0)));
    expect(find.text('Autonomous Ops Report'), findsOneWidget);
    expect(find.text('Human interventions: 0'), findsOneWidget);
    expect(tester.widget<Text>(find.byKey(const ValueKey('report-timestamp'))).data,
        'Generated 06:00 UTC · 11:30 IST · on_demand');
    expect(find.byType(Table), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('report-generate')));
    await tester.pumpAndSettle();
    expect(tester.widget<Text>(find.byKey(const ValueKey('report-timestamp'))).data, contains('manual'));
    await tester.pumpWidget(const SizedBox()); // disposes the poll timer
  });

  testWidgets('Report tab marks an LLM-written summary', (tester) async {
    await _pump(tester, DioReportApi(apiBaseUrl: 'http://gw', dio: Dio()..httpClientAdapter = _Adapter()));
    expect(tester.widget<Text>(find.byKey(const ValueKey('report-timestamp'))).data, contains('LLM summary'));
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('Report tab shows an error when the service is down', (tester) async {
    await _pump(tester, _FailingApi());
    expect(find.textContaining('ops report'), findsOneWidget);
    await tester.tap(find.byKey(const ValueKey('report-generate')));
    await tester.pumpAndSettle();
    expect(find.textContaining('generate a report'), findsOneWidget);
    await tester.pumpWidget(const SizedBox());
  });

  testWidgets('SimpleMarkdown renders headings, tables, bullets, notes and inline styles', (tester) async {
    await tester.pumpWidget(const MaterialApp(home: Scaffold(body: SingleChildScrollView(child: SimpleMarkdown(_md)))));
    expect(find.text('Executive summary'), findsOneWidget);
    expect(find.text('Generated 2026-11-20 06:00 UTC · trigger: manual'), findsOneWidget);
    expect(find.byType(Table), findsOneWidget);
    expect(find.text('•  '), findsOneWidget);
    expect(find.text('◦  '), findsOneWidget);
    final spans = SimpleMarkdown.inline('a **b** `c` d');
    expect(spans.length, 5);
    expect((spans[1] as TextSpan).style!.fontWeight, FontWeight.w700);
    expect((spans[3] as TextSpan).style!.fontFamily, 'monospace');
    await tester.pumpWidget(const MaterialApp(home: Scaffold(body: SimpleMarkdown('| a |\n|---|\n\n### h3\n'))));
    expect(find.text('h3'), findsOneWidget);
  });
}
