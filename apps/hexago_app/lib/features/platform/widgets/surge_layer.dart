import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:latlong2/latlong.dart' show LatLng;

import '../../../theme/hexago_theme.dart';
import '../models/platform_models.dart';
import '../state/platform_providers.dart';

/// Amber at 1.0x, red at 2.5x (the cap).
Color surgeColor(double multiplier) {
  final t = ((multiplier - 1) / 1.5).clamp(0.0, 1.0);
  return Color.lerp(HexaColors.amber, HexaColors.bad, t)!;
}

/// Translucent hexes for every surge-map cell above 1.0x, each labelled with
/// its multiplier ("1.4x"). A flutter_map layer: put it in `FlutterMap.children`.
/// Hidden when the legend's surge toggle is off.
class SurgeZonesLayer extends ConsumerWidget {
  const SurgeZonesLayer({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    if (!ref.watch(showSurgeProvider)) return const SizedBox.shrink();
    final data = ref.watch(surgeMapProvider).value;
    final cells = data?.surging ?? const <SurgeCell>[];
    if (cells.isEmpty) return const SizedBox.shrink();
    return Stack(
      key: const ValueKey('surge-zones'),
      children: [
        IgnorePointer(
          child: PolygonLayer(
            polygons: [
              for (final c in cells.reversed) // highest painted last, on top
                Polygon(
                  points: [for (final (lat, lng) in c.boundary) LatLng(lat, lng)],
                  color: surgeColor(c.multiplier)
                      .withValues(alpha: 0.18 + 0.12 * ((c.multiplier - 1) / 1.5).clamp(0, 1)),
                  borderColor: surgeColor(c.multiplier).withValues(alpha: 0.85),
                  borderStrokeWidth: 1.4,
                ),
            ],
          ),
        ),
        MarkerLayer(
          markers: [
            for (final c in cells)
              Marker(
                point: LatLng(c.centerLat, c.centerLng),
                width: 46,
                height: 22,
                child: Tooltip(
                  message: c.explanation.isNotEmpty
                      ? c.explanation
                      : '${c.label}: ${c.demand} requests vs ${c.supply} idle cars in the last 5 min',
                  child: Container(
                    key: ValueKey('surge-label-${c.cell}'),
                    alignment: Alignment.center,
                    decoration: BoxDecoration(
                      color: const Color(0xDD0B0F17),
                      borderRadius: BorderRadius.circular(6),
                      border: Border.all(color: surgeColor(c.multiplier)),
                    ),
                    child: Text(
                      c.label,
                      style: TextStyle(fontSize: 11, fontWeight: FontWeight.w800, color: surgeColor(c.multiplier)),
                    ),
                  ),
                ),
              ),
          ],
        ),
      ],
    );
  }
}

/// Legend switch for the surge zones, with the number of surging cells.
class SurgeToggle extends ConsumerWidget {
  const SurgeToggle({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final on = ref.watch(showSurgeProvider);
    final n = ref.watch(surgeMapProvider).value?.surging.length ?? 0;
    final c = on ? HexaColors.amber : HexaColors.textMuted;
    return InkWell(
      key: const ValueKey('surge-toggle'),
      borderRadius: BorderRadius.circular(6),
      onTap: () => ref.read(showSurgeProvider.notifier).toggle(),
      child: Padding(
        padding: const EdgeInsets.symmetric(horizontal: 2, vertical: 1),
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            Icon(on ? Icons.check_box : Icons.check_box_outline_blank, size: 13, color: c),
            const SizedBox(width: 3),
            Container(
              width: 11,
              height: 9,
              decoration: BoxDecoration(
                gradient: const LinearGradient(colors: [HexaColors.amber, HexaColors.bad]),
                borderRadius: BorderRadius.circular(2),
              ),
            ),
            const SizedBox(width: 4),
            Text(
              n > 0 ? 'Surge zones ($n)' : 'Surge zones',
              key: const ValueKey('surge-toggle-label'),
              style: TextStyle(fontSize: 10.5, color: on ? null : c),
            ),
          ],
        ),
      ),
    );
  }
}
