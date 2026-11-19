import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../theme/hexago_theme.dart';
import '../../../util/errors.dart';
import '../../../util/format.dart';
import '../../../widgets/waking_gate.dart';
import '../../money/models/ledger_models.dart' show formatRupees;
import '../../money/money_widgets.dart' show showFareBreakdownSheet;
import '../models/driver_models.dart';
import '../state/driver_providers.dart';

/// Today's trips, total earnings, payouts (paid / held for review) and wallet
/// balance from the ledger; tap a trip for its fare split.
class DriverEarningsView extends ConsumerStatefulWidget {
  const DriverEarningsView({super.key, required this.driverId, this.now});

  final String driverId;
  final DateTime Function()? now;

  @override
  ConsumerState<DriverEarningsView> createState() => _DriverEarningsViewState();
}

class _DriverEarningsViewState extends ConsumerState<DriverEarningsView> {
  EarningsToday? _today;
  DriverWallet? _wallet;
  Object? _error;
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    unawaited(_load());
    _timer = Timer.periodic(ref.read(earningsPollProvider), (_) => _load());
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  Future<void> _load() async {
    final api = ref.read(driverApiProvider);
    try {
      final payouts = await api.payouts(widget.driverId);
      // AV earnings are paid to the fleet operator: show that owner's wallet.
      final owner = payouts.isNotEmpty && payouts.first.owner.isNotEmpty ? payouts.first.owner : widget.driverId;
      final wallet = await api.wallet(owner);
      if (!mounted) return;
      setState(() {
        _today = EarningsToday.from(payouts, (widget.now ?? DateTime.now)());
        _wallet = wallet;
        _error = null;
      });
    } catch (e) {
      if (!mounted || WakingGate.report(context, e)) return;
      setState(() => _error = e);
    }
  }

  @override
  Widget build(BuildContext context) {
    final t = _today;
    if (t == null) {
      return Center(
        child: _error == null
            ? const CircularProgressIndicator()
            : Text(friendlyError(_error, action: 'load earnings'), style: const TextStyle(color: HexaColors.bad)),
      );
    }
    final w = _wallet;
    return ListView(
      key: const ValueKey('earnings-list'),
      padding: const EdgeInsets.all(16),
      children: [
        Wrap(spacing: 10, runSpacing: 10, children: [
          _Stat(id: 'trips', label: "Today's trips", value: '${t.trips}'),
          _Stat(id: 'total', label: 'Earned today', value: formatRupees(t.totalPaise), color: HexaColors.good),
          _Stat(id: 'paid', label: 'Paid out', value: formatRupees(t.paidPaise)),
          _Stat(id: 'held', label: 'Held for review', value: formatRupees(t.heldPaise), color: t.heldPaise > 0 ? HexaColors.warn : null),
          _Stat(
            id: 'wallet',
            label: w != null && w.owner != widget.driverId ? 'Wallet (${w.owner})' : 'Wallet balance',
            value: w == null ? '–' : formatRupees(w.balancePaise),
          ),
        ]),
        const SizedBox(height: 16),
        const Text('Trips today', style: TextStyle(fontWeight: FontWeight.w700)),
        const SizedBox(height: 6),
        if (t.payouts.isEmpty)
          const Padding(
            key: ValueKey('earnings-empty'),
            padding: EdgeInsets.symmetric(vertical: 20),
            child: Text('No completed trips yet today.', style: TextStyle(color: HexaColors.textMuted)),
          ),
        for (final p in t.payouts) _TripTile(payout: p),
      ],
    );
  }
}

class _TripTile extends StatelessWidget {
  const _TripTile({required this.payout});

  final DriverPayout payout;

  @override
  Widget build(BuildContext context) {
    final p = payout;
    final c = p.held ? HexaColors.warn : (p.status == 'sent' ? HexaColors.good : HexaColors.textMuted);
    return Card(
      key: ValueKey('trip-${p.rideId}'),
      margin: const EdgeInsets.only(bottom: 6),
      child: ListTile(
        onTap: () => showFareBreakdownSheet(context, p.rideId),
        leading: Icon(p.held ? Icons.pause_circle_outline : Icons.check_circle_outline, color: c),
        title: Row(children: [
          Text(shortId(p.rideId), style: const TextStyle(fontFamily: 'monospace', fontWeight: FontWeight.w700)),
          const SizedBox(width: 8),
          if (p.createdAt != null)
            Text(formatClock(p.createdAt!.toLocal()), style: const TextStyle(color: HexaColors.textMuted, fontSize: 12)),
          const Spacer(),
          Text(formatRupees(p.amountPaise), style: const TextStyle(fontWeight: FontWeight.w700)),
        ]),
        subtitle: Text(
          p.held && p.incidentId.isNotEmpty ? '${p.statusLabel} · incident ${p.incidentId}' : p.statusLabel,
          key: ValueKey('trip-status-${p.rideId}'),
          style: TextStyle(color: c, fontSize: 12),
        ),
        trailing: const Tooltip(message: 'Fare split', child: Icon(Icons.receipt_long, size: 18)),
      ),
    );
  }
}

class _Stat extends StatelessWidget {
  const _Stat({required this.id, required this.label, required this.value, this.color});

  final String id;
  final String label;
  final String value;
  final Color? color;

  @override
  Widget build(BuildContext context) => Container(
        width: 160,
        padding: const EdgeInsets.all(12),
        decoration: BoxDecoration(
          color: HexaColors.panel,
          borderRadius: BorderRadius.circular(10),
          border: Border.all(color: HexaColors.border),
        ),
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          Text(label, style: const TextStyle(color: HexaColors.textMuted, fontSize: 11.5), overflow: TextOverflow.ellipsis),
          const SizedBox(height: 4),
          Text(value,
              key: ValueKey('earnings-$id'),
              style: TextStyle(fontSize: 18, fontWeight: FontWeight.w800, color: color, fontFeatures: const [FontFeature.tabularFigures()])),
        ]),
      );
}
