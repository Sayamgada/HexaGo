import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart' show LatLng;

import '../../../theme/hexago_theme.dart';
import '../models/takeover_models.dart';

/// Cyan (quiet) -> amber -> red (hot), on the forecast's absolute 0..1 score.
Color demandColor(double score) {
  final s = score.clamp(0.0, 1.0);
  return s < 0.5
      ? Color.lerp(HexaColors.cyan, HexaColors.amber, s / 0.5)!
      : Color.lerp(HexaColors.amber, HexaColors.bad, (s - 0.5) / 0.5)!;
}

/// The Demand Forecast heatmap: one translucent hex per H3 cell, coloured by
/// demand. A flutter_map layer: put it in `FlutterMap.children`.
class ForecastLayer extends StatelessWidget {
  const ForecastLayer({super.key, required this.cells, this.selected});

  final List<ForecastCell> cells;
  final String? selected;

  @override
  Widget build(BuildContext context) {
    final shown = [for (final c in cells) if (c.demandScore >= 0.05 && c.boundary.length >= 3) c];
    if (shown.isEmpty) return const SizedBox.shrink();
    return IgnorePointer(
      key: const ValueKey('forecast-layer'),
      child: PolygonLayer(polygons: [
        for (final c in shown.reversed) // hottest painted last, on top
          Polygon(
            points: [for (final (lat, lng) in c.boundary) LatLng(lat, lng)],
            color: demandColor(c.demandScore).withValues(alpha: 0.10 + 0.32 * c.demandScore),
            borderColor: c.h3 == selected ? Colors.white : demandColor(c.demandScore).withValues(alpha: 0.7),
            borderStrokeWidth: c.h3 == selected ? 2.6 : 1.0,
          ),
      ]),
    );
  }
}

/// Now / +15 / +30 slider for the heatmap.
class HorizonSelector extends StatelessWidget {
  const HorizonSelector({super.key, required this.value, required this.onChanged});

  final int value;
  final ValueChanged<int> onChanged;

  @override
  Widget build(BuildContext context) => SegmentedButton<int>(
        key: const ValueKey('horizon-selector'),
        showSelectedIcon: false,
        style: const ButtonStyle(visualDensity: VisualDensity.compact),
        segments: [
          for (final h in forecastHorizons)
            ButtonSegment(value: h, label: Text(horizonLabel(h), key: ValueKey('horizon-$h'))),
        ],
        selected: {value},
        onSelectionChanged: (s) => onChanged(s.first),
      );
}

/// Heat legend for the map overlay.
class DemandLegend extends StatelessWidget {
  const DemandLegend({super.key});

  @override
  Widget build(BuildContext context) => Row(mainAxisSize: MainAxisSize.min, children: [
        const Text('Demand', style: TextStyle(fontSize: 11, color: HexaColors.textMuted)),
        const SizedBox(width: 6),
        for (final s in [0.1, 0.35, 0.6, 0.85])
          Container(width: 14, height: 8, margin: const EdgeInsets.only(right: 2), color: demandColor(s)),
      ]);
}

/// "Move to hotspot": the top cells by expected earnings, with distance and
/// the Demand Forecast agent's explanation. Tapping one draws the route.
class HotspotCards extends StatelessWidget {
  const HotspotCards({super.key, required this.hotspots, required this.onTap, this.selected, this.loading = false});

  final List<Hotspot> hotspots;
  final ValueChanged<Hotspot> onTap;
  final String? selected;
  final bool loading;

  @override
  Widget build(BuildContext context) {
    if (hotspots.isEmpty) {
      return Text(loading ? 'Loading the demand forecast...' : 'No demand hotspots forecast right now.',
          key: const ValueKey('hotspots-empty'), style: const TextStyle(color: HexaColors.textMuted, fontSize: 12));
    }
    return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
      for (final (i, h) in hotspots.indexed) ...[
        if (i > 0) const SizedBox(height: 6),
        _card(h, i + 1),
      ],
    ]);
  }

  Widget _card(Hotspot h, int rank) {
    final c = h.cell;
    final on = c.h3 == selected;
    final color = demandColor(c.demandScore);
    return Material(
      color: on ? color.withValues(alpha: 0.16) : HexaColors.panel,
      borderRadius: BorderRadius.circular(10),
      child: InkWell(
        key: ValueKey('hotspot-${c.h3}'),
        borderRadius: BorderRadius.circular(10),
        onTap: () => onTap(h),
        child: Container(
          padding: const EdgeInsets.all(10),
          decoration: BoxDecoration(
            borderRadius: BorderRadius.circular(10),
            border: Border.all(color: on ? color : HexaColors.border),
          ),
          child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Row(children: [
              CircleAvatar(radius: 10, backgroundColor: color, child: Text('$rank', style: const TextStyle(fontSize: 11, color: Colors.black))),
              const SizedBox(width: 8),
              Text('${h.distanceKm.toStringAsFixed(1)} km away', style: const TextStyle(fontWeight: FontWeight.w700)),
              const Spacer(),
              Text('₹${c.earningsPerHr}/h',
                  key: ValueKey('hotspot-earn-${c.h3}'), style: const TextStyle(color: HexaColors.good, fontWeight: FontWeight.w800)),
            ]),
            const SizedBox(height: 6),
            Text(c.explanation, style: const TextStyle(fontSize: 12)),
            const SizedBox(height: 4),
            Row(children: [
              Text('${c.requestsPerHr.toStringAsFixed(0)} rides/h · ${c.trend}',
                  style: const TextStyle(fontSize: 11, color: HexaColors.textMuted)),
              const Spacer(),
              if (c.fromLlm)
                const Text('AI explanation', style: TextStyle(fontSize: 10, color: HexaColors.cyan)),
              const SizedBox(width: 6),
              Icon(on ? Icons.route : Icons.near_me_outlined, size: 14, color: on ? color : HexaColors.textMuted),
            ]),
          ]),
        ),
      ),
    );
  }
}
