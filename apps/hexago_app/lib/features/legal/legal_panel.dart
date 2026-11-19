import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../config/app_config.dart';
import '../../theme/hexago_theme.dart';
import '../../util/errors.dart';
import 'legal_api.dart';

final legalApiProvider = Provider<LegalApi>(
  (ref) => AppConfig.useMock ? MockLegalApi() : DioLegalApi(apiBaseUrl: AppConfig.apiBaseUrl),
);

/// Ops console "Legal" tab: each city's rules, a regulation text box (sample
/// Karnataka notice pre-filled) and "Ingest" -> the old -> new diff, applied.
class LegalPanel extends ConsumerStatefulWidget {
  const LegalPanel({super.key});

  @override
  ConsumerState<LegalPanel> createState() => _LegalPanelState();
}

class _LegalPanelState extends ConsumerState<LegalPanel> {
  late final LegalApi _api = ref.read(legalApiProvider);
  final _text = TextEditingController(text: defaultSampleNotice);
  JurisdictionList? _list;
  List<AuditEntry> _audit = const [];
  String? _city;
  String? _error;
  IngestResult? _result;
  bool _busy = false;

  @override
  void initState() {
    super.initState();
    unawaited(_load());
  }

  @override
  void dispose() {
    _text.dispose();
    super.dispose();
  }

  Future<void> _load() async {
    try {
      final l = await _api.jurisdictions();
      final a = await _api.audit(limit: 5);
      if (!mounted) return;
      setState(() {
        final first = _list == null;
        _list = l;
        _audit = a;
        _error = null;
        _city ??= l.pricingCity;
        if (first && _text.text == defaultSampleNotice) _text.text = l.sampleNotice;
      });
    } catch (e) {
      if (mounted) setState(() => _error = friendlyError(e, action: 'load the jurisdiction rules'));
    }
  }

  Future<void> _ingest() async {
    final text = _text.text.trim();
    if (text.isEmpty || _busy) return;
    setState(() => _busy = true);
    try {
      final r = await _api.ingest(text);
      if (!mounted) return;
      setState(() => (_result = r, _city = r.city, _error = null));
      await _load();
    } catch (e) {
      if (mounted) setState(() => _error = friendlyError(e, action: 'ingest the regulation'));
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final l = _list;
    final j = l == null ? null : (l.byCity(_city ?? l.pricingCity) ?? (l.jurisdictions.isEmpty ? null : l.jurisdictions.first));
    return ListView(
      key: const ValueKey('legal-panel'),
      padding: const EdgeInsets.all(4),
      children: [
        Row(children: [
          const Icon(Icons.gavel, size: 18, color: HexaColors.amber),
          const SizedBox(width: 8),
          Text('Legal & Jurisdiction', style: Theme.of(context).textTheme.titleSmall),
          const Spacer(),
          if (l != null && !l.enabled) const _Badge('DISABLED', HexaColors.textMuted),
        ]),
        const SizedBox(height: 8),
        if (_error != null) Padding(padding: const EdgeInsets.only(bottom: 8), child: Text(_error!, style: const TextStyle(color: HexaColors.bad))),
        if (l == null && _error == null) const Padding(padding: EdgeInsets.all(16), child: Center(child: CircularProgressIndicator())),
        if (l != null) ...[
          Wrap(spacing: 6, children: [
            for (final c in l.jurisdictions)
              ChoiceChip(
                key: ValueKey('legal-city-${c.city}'),
                label: Text(c.name),
                selected: c.city == j?.city,
                onSelected: (_) => setState(() => _city = c.city),
              ),
          ]),
          const SizedBox(height: 8),
          if (j != null) _RulesCard(j: j, pricing: j.city == l.pricingCity),
        ],
        const SizedBox(height: 12),
        Text('Regulation text', style: Theme.of(context).textTheme.labelLarge),
        const SizedBox(height: 6),
        TextField(
          key: const ValueKey('legal-text'),
          controller: _text,
          minLines: 3,
          maxLines: 6,
          style: const TextStyle(fontSize: 13),
          decoration: const InputDecoration(border: OutlineInputBorder(), isDense: true),
        ),
        const SizedBox(height: 8),
        Row(children: [
          FilledButton.icon(
            key: const ValueKey('legal-ingest'),
            onPressed: _busy || (l != null && !l.enabled) ? null : _ingest,
            icon: _busy
                ? const SizedBox(width: 14, height: 14, child: CircularProgressIndicator(strokeWidth: 2))
                : const Icon(Icons.auto_fix_high, size: 16),
            label: const Text('Ingest'),
          ),
          const SizedBox(width: 10),
          const Expanded(
            child: Text('Applied at once; the surge map picks up a new cap within 30 s.',
                style: TextStyle(fontSize: 11, color: HexaColors.textMuted)),
          ),
        ]),
        if (_result != null) ...[const SizedBox(height: 10), _ResultCard(r: _result!)],
        if (_audit.isNotEmpty) ...[
          const SizedBox(height: 14),
          Text('Audit log', style: Theme.of(context).textTheme.labelLarge),
          for (final a in _audit)
            Padding(
              padding: const EdgeInsets.only(top: 4),
              child: Text(
                '${a.createdAt == null ? '' : '${_hhmm(a.createdAt!)} · '}${a.city} · '
                '${a.diff.isEmpty ? 'no change' : a.diff.map((d) => '${ruleLabel(d.field)} ${formatRule(d.field, d.oldValue)} → ${formatRule(d.field, d.newValue)}').join('; ')}'
                ' · ${a.source}',
                style: const TextStyle(fontSize: 11, color: HexaColors.textMuted),
              ),
            ),
        ],
      ],
    );
  }
}

String _hhmm(DateTime t) {
  final u = t.toUtc();
  return '${u.hour.toString().padLeft(2, '0')}:${u.minute.toString().padLeft(2, '0')} UTC';
}

class _Badge extends StatelessWidget {
  const _Badge(this.text, this.color);

  final String text;
  final Color color;

  @override
  Widget build(BuildContext context) => Container(
        padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
        decoration: BoxDecoration(
            color: color.withValues(alpha: 0.15), borderRadius: BorderRadius.circular(4), border: Border.all(color: color)),
        child: Text(text, style: TextStyle(fontSize: 10, fontWeight: FontWeight.w700, color: color)),
      );
}

class _RulesCard extends StatelessWidget {
  const _RulesCard({required this.j, required this.pricing});

  final Jurisdiction j;
  final bool pricing;

  @override
  Widget build(BuildContext context) {
    final r = j.rules;
    Widget row(String label, String value, {Key? key}) => Padding(
          padding: const EdgeInsets.symmetric(vertical: 3),
          child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
            SizedBox(width: 140, child: Text(label, style: const TextStyle(fontSize: 12, color: HexaColors.textMuted))),
            Expanded(child: Text(value, key: key, style: const TextStyle(fontSize: 13))),
          ]),
        );
    return Container(
      key: ValueKey('legal-rules-${j.city}'),
      padding: const EdgeInsets.all(10),
      decoration: BoxDecoration(
          color: HexaColors.panel, borderRadius: BorderRadius.circular(10), border: Border.all(color: HexaColors.border)),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Row(children: [
          Expanded(
            child: Text('${j.name}${j.authority.isEmpty ? '' : ' · ${j.authority}'}',
                style: const TextStyle(fontWeight: FontWeight.w600)),
          ),
          if (pricing) const _Badge('PRICING CITY', HexaColors.cyan),
        ]),
        const SizedBox(height: 6),
        row('Surge cap', formatRule('max_surge_multiplier', r.maxSurge), key: ValueKey('legal-cap-${j.city}')),
        row('Min insurance', formatRule('min_insurance_coverage', r.minInsurance)),
        row('Max driving hours', formatRule('max_driving_hours', r.maxDrivingHours)),
        row('City levy', formatRule('city_levy_pct', r.cityLevyPct)),
        row('AV zones', r.avZones.isEmpty ? 'none (AVs not permitted)' : '${r.avZones.length} H3 cells: ${r.avZones.join(', ')}'),
        row('Required documents', r.documents.isEmpty ? 'none' : r.documents.join(', ')),
        const SizedBox(height: 4),
        Text(
          'v${j.version}${j.updatedAt == null ? ' · seed' : ' · updated ${_hhmm(j.updatedAt!)} by ${j.updatedBy}'}',
          style: const TextStyle(fontSize: 11, color: HexaColors.textMuted),
        ),
      ]),
    );
  }
}

class _ResultCard extends StatelessWidget {
  const _ResultCard({required this.r});

  final IngestResult r;

  @override
  Widget build(BuildContext context) {
    final color = r.applied ? HexaColors.good : HexaColors.textMuted;
    return Container(
      key: const ValueKey('legal-result'),
      padding: const EdgeInsets.all(10),
      decoration: BoxDecoration(
          color: color.withValues(alpha: 0.08), borderRadius: BorderRadius.circular(10), border: Border.all(color: color)),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Row(children: [
          _Badge(r.applied ? 'APPLIED' : 'NO CHANGE', color),
          const SizedBox(width: 6),
          _Badge(r.source == 'llm' ? 'LLM' : 'RULES', r.source == 'llm' ? HexaColors.cyan : HexaColors.amber),
          const SizedBox(width: 6),
          Expanded(
            child: Text(
              '${r.jurisdiction.name} v${r.jurisdiction.version}${r.usedFallback && r.fallbackReason != null ? ' · fallback: ${r.fallbackReason}' : ''}',
              style: const TextStyle(fontSize: 11, color: HexaColors.textMuted),
            ),
          ),
        ]),
        const SizedBox(height: 6),
        for (final d in r.diff)
          Padding(
            padding: const EdgeInsets.symmetric(vertical: 2),
            child: Text.rich(
              key: ValueKey('legal-diff-${d.field}'),
              TextSpan(children: [
                TextSpan(text: '${ruleLabel(d.field)}  ', style: const TextStyle(color: HexaColors.textMuted)),
                TextSpan(
                    text: formatRule(d.field, d.oldValue),
                    style: const TextStyle(decoration: TextDecoration.lineThrough, color: HexaColors.textMuted)),
                const TextSpan(text: '  →  '),
                TextSpan(text: formatRule(d.field, d.newValue), style: const TextStyle(fontWeight: FontWeight.w700)),
              ]),
            ),
          ),
        for (final x in r.rejected)
          Text('Rejected ${ruleLabel('${x['field']}')}: ${x['value']} (${x['reason']})',
              style: const TextStyle(fontSize: 12, color: HexaColors.bad)),
        if (r.summary.isNotEmpty) ...[
          const SizedBox(height: 4),
          Text(r.summary, style: const TextStyle(fontSize: 12)),
        ],
      ]),
    );
  }
}
