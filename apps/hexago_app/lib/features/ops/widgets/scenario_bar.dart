import 'package:flutter/material.dart';

import '../../../theme/hexago_theme.dart';
import '../models/ops_models.dart';

IconData scenarioIcon(Scenario s) => switch (s) {
      Scenario.rushHour => Icons.traffic,
      Scenario.rainStorm => Icons.thunderstorm_outlined,
      Scenario.concertExit => Icons.stadium_outlined,
      Scenario.safetyIncident => Icons.health_and_safety_outlined,
    };

/// What-if buttons. Each button is disabled while its request is in flight.
class ScenarioBar extends StatefulWidget {
  const ScenarioBar({super.key, required this.onTrigger});

  final Future<void> Function(Scenario) onTrigger;

  @override
  State<ScenarioBar> createState() => _ScenarioBarState();
}

class _ScenarioBarState extends State<ScenarioBar> {
  final Set<Scenario> _busy = {};

  Future<void> _run(Scenario s) async {
    setState(() => _busy.add(s));
    try {
      await widget.onTrigger(s);
    } finally {
      if (mounted) setState(() => _busy.remove(s));
    }
  }

  @override
  Widget build(BuildContext context) {
    return Wrap(
      spacing: 8,
      runSpacing: 8,
      children: [
        for (final s in Scenario.values)
          FilledButton.tonalIcon(
            key: ValueKey('scenario-${s.wire}'),
            style: FilledButton.styleFrom(
              backgroundColor: (s == Scenario.safetyIncident ? HexaColors.bad : HexaColors.panel).withValues(
                alpha: s == Scenario.safetyIncident ? 0.25 : 0.92,
              ),
              foregroundColor: Colors.white,
              side: BorderSide(color: s == Scenario.safetyIncident ? HexaColors.bad : HexaColors.border),
              padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
              visualDensity: VisualDensity.compact,
            ),
            onPressed: _busy.contains(s) ? null : () => _run(s),
            icon: _busy.contains(s)
                ? const SizedBox(width: 14, height: 14, child: CircularProgressIndicator(strokeWidth: 2))
                : Icon(scenarioIcon(s), size: 16),
            label: Text(s.label, style: const TextStyle(fontSize: 12.5)),
          ),
      ],
    );
  }
}
