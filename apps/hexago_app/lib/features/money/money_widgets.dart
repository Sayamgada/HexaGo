import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../config/app_config.dart';
import '../../theme/hexago_theme.dart';
import '../../util/format.dart';
import 'data/ledger_api.dart';
import 'models/ledger_models.dart';

/// Mock (offline demo) or the live gateway, like the Ops and Safety sources.
final ledgerApiProvider =
    Provider<LedgerApi>((ref) => AppConfig.useMock ? MockLedgerApi() : DioLedgerApi(apiBaseUrl: AppConfig.apiBaseUrl));

/// Ops "Money" tile: live trial balance (always ₹0.00 → green "Balanced"),
/// payouts sent today, payouts held for safety review, platform revenue.
class MoneyTile extends ConsumerStatefulWidget {
  const MoneyTile({super.key, this.pollEvery = const Duration(seconds: 5)});

  final Duration pollEvery;

  @override
  ConsumerState<MoneyTile> createState() => _MoneyTileState();
}

class _MoneyTileState extends ConsumerState<MoneyTile> {
  late final LedgerApi _api = ref.read(ledgerApiProvider);
  LedgerSummary? _summary;
  Object? _error;
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
      final s = await _api.summary();
      if (mounted) {
        setState(() {
          _summary = s;
          _error = null;
        });
      }
    } catch (e) {
      if (mounted) setState(() => _error = e);
    }
  }

  @override
  Widget build(BuildContext context) {
    final s = _summary;
    final balanced = s?.balanced ?? false;
    final balanceColor = s == null ? HexaColors.textMuted : (balanced ? HexaColors.good : HexaColors.bad);
    final held = s?.heldCount ?? 0;
    return Container(
      key: const ValueKey('money-tile'),
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
      decoration: BoxDecoration(
        color: HexaColors.panel,
        borderRadius: BorderRadius.circular(10),
        border: Border.all(color: s == null ? HexaColors.border : balanceColor.withValues(alpha: 0.5)),
      ),
      child: Wrap(
        spacing: 22,
        runSpacing: 8,
        crossAxisAlignment: WrapCrossAlignment.center,
        children: [
          Row(mainAxisSize: MainAxisSize.min, children: [
            const Icon(Icons.account_balance_wallet_outlined, size: 15, color: HexaColors.textMuted),
            const SizedBox(width: 6),
            const Text('Money', style: TextStyle(fontWeight: FontWeight.w700, fontSize: 13)),
            const SizedBox(width: 10),
            Container(
              key: const ValueKey('money-balance'),
              padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
              decoration: BoxDecoration(
                color: balanceColor.withValues(alpha: 0.12),
                borderRadius: BorderRadius.circular(6),
                border: Border.all(color: balanceColor.withValues(alpha: 0.6)),
              ),
              child: Row(mainAxisSize: MainAxisSize.min, children: [
                Icon(balanced ? Icons.check_circle : Icons.error_outline, size: 13, color: balanceColor),
                const SizedBox(width: 4),
                Text(
                  s == null
                      ? (_error == null ? 'Loading ledger…' : 'Ledger unavailable')
                      : balanced
                          ? 'Balanced ${formatRupees(s.trialBalancePaise)}'
                          : 'Unbalanced ${formatRupees(s.trialBalancePaise)}',
                  key: const ValueKey('money-balance-text'),
                  style: TextStyle(color: balanceColor, fontWeight: FontWeight.w700, fontSize: 12),
                ),
              ]),
            ),
          ]),
          _Figure(
            id: 'payouts',
            label: 'Payouts today',
            value: s == null ? '—' : '${formatRupees(s.payoutsSentPaise)} · ${formatCount(s.payoutsSentCount)}',
          ),
          _Figure(
            id: 'held',
            label: 'Held for review',
            value: s == null ? '—' : '${formatRupees(s.heldPaise)} · ${formatCount(held)}',
            color: held > 0 ? HexaColors.warn : null,
          ),
          _Figure(id: 'revenue', label: 'Platform revenue', value: s == null ? '—' : formatRupees(s.platformRevenuePaise)),
          _Figure(
            id: 'rides',
            label: s?.ridesSettledToday == null ? 'Rides settled' : 'Rides settled today',
            value: s == null ? '—' : formatCount(s.ridesSettledToday ?? s.ridesSettled),
          ),
        ],
      ),
    );
  }
}

class _Figure extends StatelessWidget {
  const _Figure({required this.id, required this.label, required this.value, this.color});

  final String id;
  final String label;
  final String value;
  final Color? color;

  @override
  Widget build(BuildContext context) {
    return Column(crossAxisAlignment: CrossAxisAlignment.start, mainAxisSize: MainAxisSize.min, children: [
      Text(label, style: const TextStyle(fontSize: 10.5, color: HexaColors.textMuted, letterSpacing: 0.3)),
      Text(
        value,
        key: ValueKey('money-$id'),
        style: TextStyle(
          fontSize: 14,
          fontWeight: FontWeight.w700,
          color: color ?? Colors.white,
          fontFeatures: const [FontFeature.tabularFigures()],
        ),
      ),
    ]);
  }
}

/// Opens the per-ride fare breakdown (GET /v1/ledger/entries?ride_id=).
Future<void> showFareBreakdownSheet(BuildContext context, String rideId) {
  return showModalBottomSheet<void>(
    context: context,
    isScrollControlled: true,
    backgroundColor: HexaColors.surface,
    shape: const RoundedRectangleBorder(borderRadius: BorderRadius.vertical(top: Radius.circular(14))),
    builder: (_) => FractionallySizedBox(heightFactor: 0.8, child: FareBreakdownView(rideId: rideId)),
  );
}

class FareBreakdownView extends ConsumerStatefulWidget {
  const FareBreakdownView({super.key, required this.rideId});

  final String rideId;

  @override
  ConsumerState<FareBreakdownView> createState() => _FareBreakdownViewState();
}

class _FareBreakdownViewState extends ConsumerState<FareBreakdownView> {
  late final Future<RideLedger> _future = ref.read(ledgerApiProvider).ride(widget.rideId);

  @override
  Widget build(BuildContext context) {
    return FutureBuilder<RideLedger>(
      future: _future,
      builder: (context, snap) {
        final header = Padding(
          padding: const EdgeInsets.fromLTRB(16, 14, 16, 8),
          child: Row(children: [
            const Icon(Icons.receipt_long, size: 18, color: HexaColors.good),
            const SizedBox(width: 8),
            const Text('Fare breakdown', style: TextStyle(fontWeight: FontWeight.w700, fontSize: 15)),
            const SizedBox(width: 8),
            Text(shortId(widget.rideId), style: const TextStyle(fontFamily: 'monospace', color: HexaColors.textMuted)),
            const Spacer(),
            IconButton(
              key: const ValueKey('fare-close'),
              icon: const Icon(Icons.close, size: 18),
              onPressed: () => Navigator.of(context).maybePop(),
            ),
          ]),
        );
        Widget body;
        if (snap.hasError) {
          body = Center(child: Text('Could not load the ledger: ${snap.error}', style: const TextStyle(color: HexaColors.bad)));
        } else if (!snap.hasData) {
          body = const Center(child: CircularProgressIndicator());
        } else if (snap.data!.breakdown == null) {
          body = const Center(
            key: ValueKey('fare-not-settled'),
            child: Text('Not settled yet: the ride has no ledger entries.', style: TextStyle(color: HexaColors.textMuted)),
          );
        } else {
          body = _Breakdown(ledger: snap.data!);
        }
        return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [header, const Divider(height: 1), Expanded(child: body)]);
      },
    );
  }
}

class _Breakdown extends StatelessWidget {
  const _Breakdown({required this.ledger});

  final RideLedger ledger;

  @override
  Widget build(BuildContext context) {
    final b = ledger.breakdown!;
    final payoutColor = switch (b.payoutStatus) {
      'sent' => HexaColors.good,
      'held_for_review' => HexaColors.warn,
      _ => HexaColors.textMuted,
    };
    Widget row(String id, String label, int paise, {String? note, Color? color, bool strong = false}) => Padding(
          padding: const EdgeInsets.symmetric(vertical: 4),
          child: Row(children: [
            Expanded(
              child: Text.rich(TextSpan(children: [
                TextSpan(text: label, style: TextStyle(fontWeight: strong ? FontWeight.w700 : FontWeight.w500)),
                if (note != null) TextSpan(text: '  $note', style: const TextStyle(color: HexaColors.textMuted, fontSize: 11)),
              ])),
            ),
            Text(
              formatRupees(paise),
              key: ValueKey('fare-$id'),
              style: TextStyle(
                color: color ?? Colors.white,
                fontWeight: strong ? FontWeight.w700 : FontWeight.w600,
                fontFeatures: const [FontFeature.tabularFigures()],
              ),
            ),
          ]),
        );
    return ListView(padding: const EdgeInsets.fromLTRB(16, 10, 16, 20), children: [
      row('fare', 'Fare', b.farePaise, strong: true),
      if (b.promoPaise > 0) row('promo', 'Promo', b.promoPaise, note: 'funded by promo_wallet', color: HexaColors.good),
      if (b.tipPaise > 0) row('tip', 'Tip', b.tipPaise, note: '100% to the ${b.fleet ? 'operator' : 'driver'}'),
      row('rider', 'Rider paid', b.riderPaidPaise, note: 'fare − promo + tip'),
      const Divider(),
      row('payee', b.fleet ? 'Fleet operator' : 'Driver', b.payeePaise,
          note: '${b.payeeOwner} · ${b.fleet ? '80%' : '75%'}${b.tipPaise > 0 ? ' + tip' : ''}'),
      row('levy', 'City levy', b.cityLevyPaise, note: '2% · Bengaluru'),
      row('gst', 'GST', b.gstPaise, note: '5% on the platform fee'),
      row('platform', 'Platform fee', b.platformFeePaise, note: 'remainder incl. rounding'),
      if (b.refundedPaise > 0) row('refunded', 'Refunded', b.refundedPaise, note: 'reversal entries', color: HexaColors.warn),
      const SizedBox(height: 8),
      Wrap(spacing: 6, runSpacing: 6, children: [
        _Chip(
          key: const ValueKey('fare-payout-status'),
          text: b.payoutStatus == 'held_for_review'
              ? 'PAYOUT HELD FOR REVIEW'
              : 'PAYOUT ${b.payoutStatus.isEmpty ? 'NONE' : b.payoutStatus.toUpperCase()}',
          color: payoutColor,
        ),
        if (b.interrupted) _Chip(text: 'SAFETY ESCALATION ${b.incidentId}'.trim(), color: HexaColors.bad),
      ]),
      const SizedBox(height: 14),
      const Text('Journal entries', style: TextStyle(fontWeight: FontWeight.w700, fontSize: 13)),
      for (final e in ledger.entries.reversed) _EntryCard(entry: e),
    ]);
  }
}

class _EntryCard extends StatelessWidget {
  const _EntryCard({required this.entry});

  final LedgerEntry entry;

  @override
  Widget build(BuildContext context) {
    return Container(
      key: ValueKey('entry-${entry.id}'),
      margin: const EdgeInsets.only(top: 8),
      padding: const EdgeInsets.all(10),
      decoration: BoxDecoration(
        color: Colors.white.withValues(alpha: 0.03),
        borderRadius: BorderRadius.circular(8),
        border: Border.all(color: HexaColors.border),
      ),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Row(children: [
          Text(entry.kind.replaceAll('_', ' ').toUpperCase(),
              style: const TextStyle(fontSize: 10, fontWeight: FontWeight.w800, letterSpacing: 0.6, color: HexaColors.textMuted)),
          const Spacer(),
          Text(
            entry.sumPaise == 0 ? 'Σ ₹0.00 ✓' : 'Σ ${formatRupees(entry.sumPaise)}',
            style: TextStyle(fontSize: 10.5, color: entry.sumPaise == 0 ? HexaColors.good : HexaColors.bad),
          ),
        ]),
        const SizedBox(height: 4),
        for (final p in entry.postings)
          Padding(
            padding: const EdgeInsets.symmetric(vertical: 1.5),
            child: Row(children: [
              SizedBox(
                width: 22,
                child: Text(p.isDebit ? 'Dr' : 'Cr',
                    style: TextStyle(fontSize: 10.5, color: p.isDebit ? const Color(0xFF93C5FD) : const Color(0xFFFCA5A5))),
              ),
              Expanded(
                child: Text('${p.account}  ·  ${p.memo.replaceAll('_', ' ')}',
                    overflow: TextOverflow.ellipsis,
                    style: const TextStyle(fontSize: 11, fontFamily: 'monospace', color: Color(0xFFCBD5E1))),
              ),
              Text(formatRupees(p.amountPaise.abs()),
                  style: const TextStyle(fontSize: 11, fontFeatures: [FontFeature.tabularFigures()])),
            ]),
          ),
      ]),
    );
  }
}

class _Chip extends StatelessWidget {
  const _Chip({super.key, required this.text, required this.color});

  final String text;
  final Color color;

  @override
  Widget build(BuildContext context) => Container(
        padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
        decoration: BoxDecoration(
          color: color.withValues(alpha: 0.12),
          borderRadius: BorderRadius.circular(4),
          border: Border.all(color: color.withValues(alpha: 0.6)),
        ),
        child: Text(text, style: TextStyle(fontSize: 10, fontWeight: FontWeight.w800, color: color, letterSpacing: 0.5)),
      );
}
