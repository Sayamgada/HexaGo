import 'package:flutter/material.dart';

import '../../../theme/hexago_theme.dart';
import '../../../util/format.dart';
import '../models/driver_models.dart';

Color phaseColor(DriverPhase p) => switch (p) {
      DriverPhase.idle => HexaColors.idle,
      DriverPhase.toPickup => HexaColors.toPickup,
      DriverPhase.onTrip => HexaColors.onTrip,
      DriverPhase.suspended => HexaColors.bad,
      DriverPhase.incident => HexaColors.incident,
      DriverPhase.offline => HexaColors.offline,
    };

class StatusChip extends StatelessWidget {
  const StatusChip({super.key, required this.phase});

  final DriverPhase phase;

  @override
  Widget build(BuildContext context) {
    final c = phaseColor(phase);
    return Container(
      key: const ValueKey('driver-status-chip'),
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
      decoration: BoxDecoration(
        color: c.withValues(alpha: 0.15),
        borderRadius: BorderRadius.circular(20),
        border: Border.all(color: c.withValues(alpha: 0.7)),
      ),
      child: Text(phase.label, style: TextStyle(color: c, fontSize: 11.5, fontWeight: FontWeight.w700)),
    );
  }
}

class AvBadge extends StatelessWidget {
  const AvBadge({super.key});

  @override
  Widget build(BuildContext context) => Container(
        padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 1),
        decoration: BoxDecoration(color: HexaColors.cyan.withValues(alpha: 0.18), borderRadius: BorderRadius.circular(4)),
        child: const Text('AV', style: TextStyle(color: HexaColors.cyan, fontSize: 10.5, fontWeight: FontWeight.w800)),
      );
}

/// "Account frozen by Safety Sentinel – under review" with the incident id.
class FrozenBanner extends StatelessWidget {
  const FrozenBanner({super.key, required this.incidentId, this.reason = '', this.since});

  final String incidentId;
  final String reason;
  final DateTime? since;

  @override
  Widget build(BuildContext context) {
    return Container(
      key: const ValueKey('frozen-banner'),
      padding: const EdgeInsets.all(12),
      decoration: BoxDecoration(
        color: HexaColors.bad.withValues(alpha: 0.16),
        borderRadius: BorderRadius.circular(10),
        border: Border.all(color: HexaColors.bad),
      ),
      child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
        const Icon(Icons.gpp_bad, color: HexaColors.bad),
        const SizedBox(width: 10),
        Expanded(
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            const Text('Account frozen by Safety Sentinel – under review',
                style: TextStyle(color: HexaColors.bad, fontWeight: FontWeight.w800, fontSize: 15)),
            const SizedBox(height: 4),
            SelectableText(
              'Incident ${incidentId.isEmpty ? 'pending' : incidentId}',
              key: const ValueKey('frozen-incident'),
              style: const TextStyle(fontFamily: 'monospace', fontSize: 12.5),
            ),
            if (reason.isNotEmpty || since != null)
              Text(
                [if (reason.isNotEmpty) reason, if (since != null) 'since ${formatClock(since!.toLocal())}'].join(' · '),
                style: const TextStyle(color: HexaColors.textMuted, fontSize: 12),
              ),
            const SizedBox(height: 4),
            const Text('No new rides until the Ops Supervisor confirms a release. Payouts for the interrupted ride are held.',
                style: TextStyle(fontSize: 12, height: 1.3)),
          ]),
        ),
      ]),
    );
  }
}

/// The current ride: pickup, drop, rider, ETA and live progress.
class RideCard extends StatelessWidget {
  const RideCard({super.key, required this.ride});

  final CurrentRide? ride;

  String _pt(double lat, double lng) => '${lat.toStringAsFixed(4)}, ${lng.toStringAsFixed(4)}';

  @override
  Widget build(BuildContext context) {
    final r = ride;
    if (r == null) {
      return const _Panel(
        key: ValueKey('ride-card-empty'),
        child: Row(children: [
          Icon(Icons.hourglass_empty, color: HexaColors.textMuted, size: 18),
          SizedBox(width: 8),
          Expanded(child: Text('No current ride. Waiting for a dispatch offer...', style: TextStyle(color: HexaColors.textMuted))),
        ]),
      );
    }
    final c = r.onBoard ? HexaColors.onTrip : HexaColors.toPickup;
    return _Panel(
      key: const ValueKey('ride-card'),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Row(children: [
          Icon(Icons.route, color: c, size: 18),
          const SizedBox(width: 6),
          const Text('Current ride', style: TextStyle(fontWeight: FontWeight.w700)),
          const SizedBox(width: 6),
          Text(shortId(r.id), style: const TextStyle(fontFamily: 'monospace', color: HexaColors.textMuted, fontSize: 12)),
          const Spacer(),
          Text('ETA ${formatEta(r.etaSec)} ${r.legLabel}',
              key: const ValueKey('ride-eta'), style: TextStyle(color: c, fontWeight: FontWeight.w700)),
        ]),
        const SizedBox(height: 8),
        _line(Icons.person_outline, 'Rider', r.riderId),
        _line(Icons.trip_origin, 'Pickup', _pt(r.pickupLat, r.pickupLng), color: HexaColors.toPickup),
        _line(Icons.flag_outlined, 'Drop', _pt(r.dropLat, r.dropLng), color: HexaColors.onTrip),
        const SizedBox(height: 8),
        ClipRRect(
          borderRadius: BorderRadius.circular(4),
          child: LinearProgressIndicator(
            key: const ValueKey('ride-progress'),
            value: r.progress,
            minHeight: 6,
            color: c,
            backgroundColor: Colors.white10,
          ),
        ),
        const SizedBox(height: 4),
        Text('${(r.progress * 100).round()}% of the way ${r.legLabel}',
            style: const TextStyle(color: HexaColors.textMuted, fontSize: 11.5)),
      ]),
    );
  }

  Widget _line(IconData icon, String label, String value, {Color? color}) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 2),
        child: Row(children: [
          Icon(icon, size: 14, color: color ?? HexaColors.textMuted),
          const SizedBox(width: 6),
          SizedBox(width: 52, child: Text(label, style: const TextStyle(color: HexaColors.textMuted, fontSize: 12))),
          Expanded(child: Text(value, style: const TextStyle(fontSize: 12.5, fontFamily: 'monospace'))),
        ]),
      );
}

/// Battery, mechanical health and current mode of an autonomous vehicle.
class AvPanel extends StatelessWidget {
  const AvPanel({super.key, required this.status});

  final AvStatus status;

  @override
  Widget build(BuildContext context) {
    final b = status.batteryPct;
    final bc = b < 20 ? HexaColors.bad : (b < 40 ? HexaColors.warn : HexaColors.good);
    final hc = switch (status.health) {
      'ok' => HexaColors.good,
      'low_battery' => HexaColors.bad,
      _ => HexaColors.warn,
    };
    return _Panel(
      key: const ValueKey('av-panel'),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        const Row(children: [
          Icon(Icons.smart_toy_outlined, color: HexaColors.cyan, size: 18),
          SizedBox(width: 6),
          Text('Autonomous vehicle', style: TextStyle(fontWeight: FontWeight.w700)),
        ]),
        const SizedBox(height: 8),
        Row(children: [
          Icon(b < 20 ? Icons.battery_alert : Icons.battery_charging_full, size: 16, color: bc),
          const SizedBox(width: 6),
          Text('${b.round()}%', key: const ValueKey('av-battery'), style: TextStyle(color: bc, fontWeight: FontWeight.w800)),
          const SizedBox(width: 10),
          Expanded(
            child: ClipRRect(
              borderRadius: BorderRadius.circular(4),
              child: LinearProgressIndicator(value: b / 100, minHeight: 6, color: bc, backgroundColor: Colors.white10),
            ),
          ),
        ]),
        const SizedBox(height: 8),
        Row(children: [
          Icon(Icons.build_circle_outlined, size: 16, color: hc),
          const SizedBox(width: 6),
          Text(status.health == 'ok' ? 'Healthy' : status.health.replaceAll('_', ' '),
              key: const ValueKey('av-health'), style: TextStyle(color: hc, fontWeight: FontWeight.w700)),
          const SizedBox(width: 8),
          Expanded(child: Text(status.healthNote, style: const TextStyle(fontSize: 12, color: HexaColors.textMuted))),
        ]),
        const SizedBox(height: 6),
        Row(children: [
          const Icon(Icons.alt_route, size: 16, color: HexaColors.cyan),
          const SizedBox(width: 6),
          Text(status.modeLabel, key: const ValueKey('av-mode'), style: const TextStyle(fontSize: 12.5)),
        ]),
      ]),
    );
  }
}

class _Panel extends StatelessWidget {
  const _Panel({super.key, required this.child});

  final Widget child;

  @override
  Widget build(BuildContext context) => Container(
        padding: const EdgeInsets.all(12),
        decoration: BoxDecoration(
          color: HexaColors.panel,
          borderRadius: BorderRadius.circular(10),
          border: Border.all(color: HexaColors.border),
        ),
        child: child,
      );
}
