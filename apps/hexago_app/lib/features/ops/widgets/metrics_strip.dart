import 'package:flutter/material.dart';

import '../../../theme/hexago_theme.dart';
import '../../../util/format.dart';
import '../models/ops_models.dart';

/// Match latency under this is shown green.
const double kFastMatchMs = 50;

Color latencyColor(double ms) {
  if (ms < kFastMatchMs) return HexaColors.good;
  if (ms < 150) return HexaColors.warn;
  return HexaColors.bad;
}

/// Row of dispatch KPIs. Shows dashes until the first sample arrives.
class MetricsStrip extends StatelessWidget {
  const MetricsStrip({super.key, required this.metrics});

  final DispatchMetrics? metrics;

  @override
  Widget build(BuildContext context) {
    final m = metrics;
    String count(int? v) => v == null ? '—' : formatCount(v);
    final tiles = <Widget>[
      MetricTile(id: 'active', label: 'Active drivers', value: count(m?.activeDrivers), icon: Icons.directions_car_filled),
      MetricTile(id: 'idle', label: 'Idle', value: count(m?.idleDrivers), icon: Icons.local_parking),
      MetricTile(id: 'open', label: 'Open requests', value: count(m?.openRequests), icon: Icons.person_search),
      MetricTile(id: 'rides', label: 'Rides in progress', value: count(m?.ridesInProgress), icon: Icons.route),
      MetricTile(
        id: 'p50',
        label: 'Match p50',
        value: m == null ? '—' : formatLatency(m.p50Ms),
        icon: Icons.bolt,
        valueColor: m == null ? null : latencyColor(m.p50Ms),
      ),
      MetricTile(
        id: 'p99',
        label: 'Match p99',
        value: m == null ? '—' : formatLatency(m.p99Ms),
        icon: Icons.speed,
        valueColor: m == null ? null : latencyColor(m.p99Ms),
      ),
      MetricTile(id: 'matches', label: 'Total matches', value: count(m?.matchesTotal), icon: Icons.handshake),
    ];

    return LayoutBuilder(builder: (context, c) {
      const gap = 8.0;
      // 7 across on wide screens, otherwise wrap into 4/3/2 columns.
      final cols = c.maxWidth >= 1000
          ? 7
          : c.maxWidth >= 640
              ? 4
              : c.maxWidth >= 420
                  ? 3
                  : 2;
      final w = (c.maxWidth - gap * (cols - 1)) / cols;
      return Wrap(
        spacing: gap,
        runSpacing: gap,
        children: [for (final t in tiles) SizedBox(width: w.floorToDouble(), child: t)],
      );
    });
  }
}

class MetricTile extends StatelessWidget {
  const MetricTile({
    super.key,
    required this.id,
    required this.label,
    required this.value,
    required this.icon,
    this.valueColor,
  });

  final String id;
  final String label;
  final String value;
  final IconData icon;
  final Color? valueColor;

  @override
  Widget build(BuildContext context) {
    return Container(
      key: ValueKey('metric-$id'),
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
      decoration: BoxDecoration(
        color: HexaColors.panel,
        borderRadius: BorderRadius.circular(10),
        border: Border.all(color: valueColor?.withValues(alpha: 0.5) ?? HexaColors.border),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        mainAxisSize: MainAxisSize.min,
        children: [
          Row(children: [
            Icon(icon, size: 13, color: HexaColors.textMuted),
            const SizedBox(width: 5),
            Expanded(
              child: Text(
                label,
                overflow: TextOverflow.ellipsis,
                style: const TextStyle(fontSize: 11, color: HexaColors.textMuted, letterSpacing: 0.3),
              ),
            ),
          ]),
          const SizedBox(height: 4),
          FittedBox(
            fit: BoxFit.scaleDown,
            alignment: Alignment.centerLeft,
            child: Text(
              value,
              key: ValueKey('metric-value-$id'),
              style: TextStyle(
                fontSize: 22,
                fontWeight: FontWeight.w700,
                color: valueColor ?? Colors.white,
                fontFeatures: const [FontFeature.tabularFigures()],
              ),
            ),
          ),
        ],
      ),
    );
  }
}
