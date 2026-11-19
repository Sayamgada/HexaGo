import 'dart:async';
import 'dart:convert';

import 'package:dio/dio.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../config/app_config.dart';
import '../../theme/hexago_theme.dart';
import '../../util/errors.dart';
import '../../widgets/simple_markdown.dart';

/// Ops Supervisor "Autonomous Ops Report" (agents-py `/v1/ops/*`).
class OpsReport {
  const OpsReport({
    required this.id,
    required this.markdown,
    this.generatedAt,
    this.trigger = '',
    this.anomalies = 0,
    this.summarySource = 'template',
  });

  final String id;
  final String markdown;
  final DateTime? generatedAt;
  final String trigger;
  final int anomalies;
  final String summarySource;

  factory OpsReport.fromJson(Map<String, dynamic> j) => OpsReport(
        id: '${j['id'] ?? ''}',
        markdown: '${j['markdown'] ?? ''}',
        generatedAt: DateTime.tryParse('${j['generated_at'] ?? ''}'),
        trigger: '${j['trigger'] ?? ''}',
        anomalies: j['anomalies'] is List ? (j['anomalies'] as List).length : 0,
        summarySource: '${j['summary_source'] ?? 'template'}',
      );
}

abstract class ReportApi {
  /// `GET /v1/ops/report` (generated on demand when there is none yet).
  Future<OpsReport> latest();

  /// `POST /v1/ops/report/run`.
  Future<OpsReport> generate();
}

Map<String, dynamic> _json(Object? data) {
  final d = data is String ? jsonDecode(data) : data;
  return d is Map ? Map<String, dynamic>.from(d) : <String, dynamic>{};
}

class DioReportApi implements ReportApi {
  DioReportApi({required String apiBaseUrl, Dio? dio})
      : _api = apiBaseUrl.endsWith('/') ? apiBaseUrl.substring(0, apiBaseUrl.length - 1) : apiBaseUrl,
        _dio = dio ??
            Dio(BaseOptions(connectTimeout: const Duration(seconds: 60), receiveTimeout: const Duration(seconds: 60)));

  final String _api;
  final Dio _dio;

  @override
  Future<OpsReport> latest() async => OpsReport.fromJson(_json((await _dio.get<dynamic>('$_api/v1/ops/report')).data));

  @override
  Future<OpsReport> generate() async =>
      OpsReport.fromJson(_json((await _dio.post<dynamic>('$_api/v1/ops/report/run')).data));
}

/// Offline demo report (USE_MOCK, tests).
class MockReportApi implements ReportApi {
  MockReportApi({DateTime Function()? now}) : _now = now ?? DateTime.now;

  final DateTime Function() _now;
  int _n = 0;
  OpsReport? _last;

  OpsReport _make(String trigger) {
    _n++;
    final t = _now().toUtc();
    return OpsReport(
      id: 'opr_mock_$_n',
      generatedAt: t,
      trigger: trigger,
      markdown: '# Autonomous Ops Report\n'
          '_Generated ${t.toIso8601String().substring(0, 16).replaceFirst('T', ' ')} UTC · trigger: $trigger · Ops Supervisor_\n\n'
          '## Executive summary\n'
          'In the last hour 7 active agents made **1,284** decisions (3% on deterministic fallbacks); the ledger balances to ₹0.00. '
          'No anomalies detected; 0 human interventions.\n\n'
          '## Headline KPIs\n| KPI | Value |\n|---|---|\n| Matches today | 540 |\n| Rides settled today | 146 |\n'
          '| Dispatch p99 | 0.75 ms |\n\n'
          '## Anomalies and auto-actions\nNo anomalies detected.\n\n'
          '## Human interventions: 0\nEvery decision and action above was taken by an agent.\n',
    );
  }

  @override
  Future<OpsReport> latest() async => _last ??= _make('on_demand');

  @override
  Future<OpsReport> generate() async => _last = _make('manual');
}

final reportApiProvider = Provider<ReportApi>(
  (ref) => AppConfig.useMock ? MockReportApi() : DioReportApi(apiBaseUrl: AppConfig.apiBaseUrl),
);

/// Ops console "Report" tab: the latest report rendered from markdown, its
/// timestamp and a "Generate now" button. Re-reads every [pollEvery] so the
/// LLM-written executive summary shows up once it lands.
class ReportPanel extends ConsumerStatefulWidget {
  const ReportPanel({super.key, this.pollEvery = const Duration(seconds: 30)});

  final Duration pollEvery;

  @override
  ConsumerState<ReportPanel> createState() => _ReportPanelState();
}

class _ReportPanelState extends ConsumerState<ReportPanel> {
  late final ReportApi _api = ref.read(reportApiProvider);
  OpsReport? _report;
  String? _error;
  bool _busy = false;
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    unawaited(_refresh());
    _timer = Timer.periodic(widget.pollEvery, (_) => _refresh());
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  Future<void> _refresh() async {
    try {
      final r = await _api.latest();
      if (mounted) setState(() => (_report = r, _error = null));
    } catch (e) {
      if (mounted && _report == null) setState(() => _error = friendlyError(e, action: 'load the ops report'));
    }
  }

  Future<void> _generate() async {
    if (_busy) return;
    setState(() => _busy = true);
    try {
      final r = await _api.generate();
      if (mounted) setState(() => (_report = r, _error = null));
    } catch (e) {
      if (mounted) setState(() => _error = friendlyError(e, action: 'generate a report'));
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final r = _report;
    final t = r?.generatedAt?.toUtc();
    final ist = t?.add(const Duration(hours: 5, minutes: 30));
    String two(int v) => v.toString().padLeft(2, '0');
    return Column(key: const ValueKey('report-panel'), crossAxisAlignment: CrossAxisAlignment.stretch, children: [
      Row(children: [
        Expanded(
          child: Text(
            t == null
                ? 'Autonomous Ops Report'
                : 'Generated ${two(t.hour)}:${two(t.minute)} UTC · ${two(ist!.hour)}:${two(ist.minute)} IST · ${r!.trigger}'
                    '${r.summarySource == 'llm' ? ' · LLM summary' : ''}',
            key: const ValueKey('report-timestamp'),
            style: const TextStyle(fontSize: 12, color: HexaColors.textMuted),
          ),
        ),
        FilledButton.icon(
          key: const ValueKey('report-generate'),
          onPressed: _busy ? null : _generate,
          icon: _busy
              ? const SizedBox(width: 14, height: 14, child: CircularProgressIndicator(strokeWidth: 2))
              : const Icon(Icons.summarize, size: 16),
          label: const Text('Generate now'),
        ),
      ]),
      const SizedBox(height: 8),
      if (_error != null) Text(_error!, style: const TextStyle(color: HexaColors.bad)),
      Expanded(
        child: r == null
            ? (_error == null ? const Center(child: CircularProgressIndicator()) : const SizedBox.shrink())
            : SingleChildScrollView(
                key: const ValueKey('report-body'),
                padding: const EdgeInsets.only(right: 6),
                child: SimpleMarkdown(r.markdown),
              ),
      ),
    ]);
  }
}
