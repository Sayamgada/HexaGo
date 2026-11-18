import 'package:flutter/material.dart';

import '../../../theme/hexago_theme.dart';
import '../models/booking_models.dart';

String rupees(double v) => '₹${v.round()}';

/// Small tag naming who answered: the model, or "rule-based".
class AnsweredByTag extends StatelessWidget {
  const AnsweredByTag(this.by, {super.key});
  final AnsweredBy by;

  @override
  Widget build(BuildContext context) {
    final color = by.usedFallback ? Colors.amberAccent : Colors.lightBlueAccent;
    return Container(
      key: const Key('answered-by-tag'),
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 2),
      decoration: BoxDecoration(
        color: color.withValues(alpha: 0.12),
        borderRadius: BorderRadius.circular(10),
        border: Border.all(color: color.withValues(alpha: 0.6)),
      ),
      child: Row(mainAxisSize: MainAxisSize.min, children: [
        Icon(by.usedFallback ? Icons.rule : Icons.auto_awesome, size: 12, color: color),
        const SizedBox(width: 4),
        Text(by.tag, style: TextStyle(fontSize: 11, color: color)),
      ]),
    );
  }
}

/// The plan as a vertical timeline of legs with Edit / Confirm.
class PlanTimeline extends StatelessWidget {
  const PlanTimeline({
    super.key,
    required this.plan,
    required this.onConfirm,
    required this.onEdit,
    this.confirming = false,
    this.confirmed = false,
  });

  final BookingPlan plan;
  final VoidCallback onConfirm;
  final VoidCallback onEdit;
  final bool confirming;
  final bool confirmed;

  @override
  Widget build(BuildContext context) {
    return Card(
      key: Key('plan-${plan.planId}'),
      color: HexaColors.panel,
      child: Padding(
        padding: const EdgeInsets.all(14),
        child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
          Row(children: [
            Text('Your plan · ${plan.legs.length} ride${plan.legs.length == 1 ? '' : 's'}',
                style: const TextStyle(fontWeight: FontWeight.bold, fontSize: 16)),
            const Spacer(),
            AnsweredByTag(plan.answeredBy),
          ]),
          const SizedBox(height: 12),
          for (var i = 0; i < plan.legs.length; i++) _LegRow(leg: plan.legs[i], last: i == plan.legs.length - 1),
          for (final w in plan.warnings) _Warning(w),
          const Divider(height: 24),
          Row(children: [
            Text('Total ${rupees(plan.totalFare)}',
                key: const Key('plan-total'), style: const TextStyle(fontWeight: FontWeight.bold, fontSize: 16)),
            const Spacer(),
            if (!confirmed) ...[
              TextButton.icon(
                key: const Key('plan-edit'),
                onPressed: confirming ? null : onEdit,
                icon: const Icon(Icons.edit, size: 16),
                label: const Text('Edit'),
              ),
              const SizedBox(width: 8),
              FilledButton.icon(
                key: const Key('plan-confirm'),
                onPressed: confirming ? null : onConfirm,
                icon: confirming
                    ? const SizedBox(width: 14, height: 14, child: CircularProgressIndicator(strokeWidth: 2))
                    : const Icon(Icons.check, size: 16),
                label: Text(confirming ? 'Scheduling…' : 'Confirm'),
              ),
            ] else
              const Chip(avatar: Icon(Icons.event_available, size: 16), label: Text('Scheduled')),
          ]),
        ]),
      ),
    );
  }
}

class _LegRow extends StatelessWidget {
  const _LegRow({required this.leg, required this.last});
  final PlanLeg leg;
  final bool last;

  @override
  Widget build(BuildContext context) {
    final muted = TextStyle(color: Colors.white.withValues(alpha: 0.65), fontSize: 12.5);
    return IntrinsicHeight(
      key: Key('leg-${leg.seq}'),
      child: Row(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
        SizedBox(
          width: 34,
          child: Column(children: [
            CircleAvatar(
              radius: 13,
              backgroundColor: leg.passenger == 'me' ? Colors.amberAccent : Colors.tealAccent,
              child: Text('${leg.seq}', style: const TextStyle(color: Colors.black, fontSize: 12)),
            ),
            if (!last) Expanded(child: Container(width: 2, color: Colors.white24)),
          ]),
        ),
        const SizedBox(width: 8),
        Expanded(
          child: Padding(
            padding: const EdgeInsets.only(bottom: 16),
            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
              Row(children: [
                Icon(leg.passenger == 'me' ? Icons.person : Icons.child_care, size: 16),
                const SizedBox(width: 4),
                Text(leg.passengerName, style: const TextStyle(fontWeight: FontWeight.w600)),
                const Spacer(),
                Text(rupees(leg.price.total), style: const TextStyle(fontWeight: FontWeight.bold)),
              ]),
              const SizedBox(height: 4),
              Text('${leg.origin.short} → ${leg.destination.short}', style: const TextStyle(fontSize: 15)),
              const SizedBox(height: 4),
              Wrap(spacing: 10, runSpacing: 2, children: [
                Text('Pickup ${leg.pickupTime}', style: muted),
                if (leg.arriveByTime != null) Text('Arrive by ${leg.arriveByTime}', style: muted),
                if (leg.etaArrivalTime.isNotEmpty) Text('ETA ${leg.etaArrivalTime}', style: muted),
                Text(leg.tierLabel.isEmpty ? leg.tier : leg.tierLabel, style: muted),
                if (leg.durationMin > 0) Text('${leg.durationMin.round()} min', style: muted),
                if (leg.price.surging)
                  Text('${leg.price.surgeMultiplier}x surge', style: const TextStyle(color: Colors.orangeAccent, fontSize: 12.5)),
              ]),
              for (final w in leg.warnings) _Warning(w),
            ]),
          ),
        ),
      ]),
    );
  }
}

class _Warning extends StatelessWidget {
  const _Warning(this.text);
  final String text;

  @override
  Widget build(BuildContext context) => Padding(
        padding: const EdgeInsets.only(top: 4),
        child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
          const Icon(Icons.warning_amber_rounded, size: 15, color: Colors.orangeAccent),
          const SizedBox(width: 4),
          Expanded(child: Text(text, style: const TextStyle(color: Colors.orangeAccent, fontSize: 12.5))),
        ]),
      );
}
