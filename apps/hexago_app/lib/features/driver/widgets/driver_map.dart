import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:latlong2/latlong.dart' show LatLng;

import '../../../theme/hexago_theme.dart';
import '../../../widgets/hexagon.dart';
import '../../ops/state/ops_providers.dart';
import '../../platform/widgets/surge_layer.dart';
import '../models/driver_models.dart';
import '../models/takeover_models.dart';
import 'driver_widgets.dart';
import 'forecast_widgets.dart';

/// Live map centred on the driver's car, with the pickup / drop pins, a line
/// to the current milestone, the surge hex overlay and its toggle, and (in
/// take-over mode) the Demand Forecast heatmap with its Now / +15 / +30
/// selector and the route to a chosen hotspot.
class DriverMap extends ConsumerStatefulWidget {
  const DriverMap({
    super.key,
    required this.lat,
    required this.lng,
    required this.heading,
    required this.phase,
    this.ride,
    this.av = false,
    this.forecast = const [],
    this.horizon,
    this.onHorizon,
    this.selectedCell,
    this.route = const [],
    this.offer,
  });

  final double lat;
  final double lng;
  final double heading;
  final DriverPhase phase;
  final CurrentRide? ride;
  final bool av;

  /// Demand Forecast cells for the heatmap (empty = no heatmap).
  final List<ForecastCell> forecast;
  final int? horizon;
  final ValueChanged<int>? onHorizon;
  final String? selectedCell;

  /// Route to the selected hotspot.
  final List<(double, double)> route;

  /// A pending ride offer: its pickup is pinned.
  final RideOffer? offer;

  @override
  ConsumerState<DriverMap> createState() => _DriverMapState();
}

class _DriverMapState extends ConsumerState<DriverMap> {
  final _map = MapController();
  bool _ready = false;
  bool _follow = true;

  @override
  void didUpdateWidget(covariant DriverMap old) {
    super.didUpdateWidget(old);
    if (_ready && _follow && (old.lat != widget.lat || old.lng != widget.lng)) {
      _map.move(LatLng(widget.lat, widget.lng), _map.camera.zoom);
    }
  }

  @override
  void dispose() {
    _map.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final basemap = ref.watch(showMapTilesProvider) ? ref.watch(basemapProvider).value : null;
    final car = LatLng(widget.lat, widget.lng);
    final r = widget.ride;
    final target = r == null ? null : (r.onBoard ? LatLng(r.dropLat, r.dropLng) : LatLng(r.pickupLat, r.pickupLng));
    final c = phaseColor(widget.phase);
    return ClipRRect(
      borderRadius: BorderRadius.circular(12),
      child: Stack(children: [
        const Positioned.fill(
          child: ColoredBox(color: Color(0xFF0B0F17), child: HexGridBackground(color: Color(0x0FFFFFFF), radius: 22)),
        ),
        FlutterMap(
          key: const ValueKey('driver-map'),
          mapController: _map,
          options: MapOptions(
            initialCenter: car,
            initialZoom: 14,
            minZoom: 9,
            maxZoom: 18,
            backgroundColor: Colors.transparent,
            interactionOptions: const InteractionOptions(flags: InteractiveFlag.all & ~InteractiveFlag.rotate),
            onMapReady: () => _ready = true,
            onPositionChanged: (_, hasGesture) {
              if (hasGesture && _follow) setState(() => _follow = false);
            },
          ),
          children: [
            if (basemap != null)
              TileLayer(
                urlTemplate: basemap.urlTemplate,
                fallbackUrl: basemap.fallbackUrl,
                subdomains: basemap.subdomains,
                retinaMode: basemap.subdomains.isNotEmpty && RetinaMode.isHighDensity(context),
                tileBuilder: basemap.darkFilter ? darkModeTileBuilder : null,
                userAgentPackageName: 'com.hexago.hexago_app',
                maxZoom: 19,
              ),
            if (widget.forecast.isNotEmpty) ForecastLayer(cells: widget.forecast, selected: widget.selectedCell),
            const SurgeZonesLayer(),
            if (widget.route.length >= 2)
              PolylineLayer(polylines: [
                Polyline(
                  points: [for (final (a, b) in widget.route) LatLng(a, b)],
                  color: HexaColors.cyan,
                  strokeWidth: 4,
                ),
              ]),
            if (target != null)
              PolylineLayer(polylines: [
                Polyline(points: [car, target], color: c.withValues(alpha: 0.8), strokeWidth: 3, pattern: StrokePattern.dashed(segments: const [8, 6])),
              ]),
            MarkerLayer(markers: [
              if (widget.offer != null)
                Marker(
                  point: LatLng(widget.offer!.pickupLat, widget.offer!.pickupLng),
                  width: 30,
                  height: 30,
                  child: const Icon(Icons.person_pin_circle, color: HexaColors.amber, size: 28, key: ValueKey('offer-pin')),
                ),
              if (r != null) ...[
                Marker(
                  point: LatLng(r.pickupLat, r.pickupLng),
                  width: 26,
                  height: 26,
                  child: const Icon(Icons.trip_origin, color: HexaColors.toPickup, size: 22),
                ),
                Marker(
                  point: LatLng(r.dropLat, r.dropLng),
                  width: 28,
                  height: 28,
                  child: const Icon(Icons.flag, color: HexaColors.onTrip, size: 24),
                ),
              ],
              Marker(
                point: car,
                width: 40,
                height: 40,
                child: Transform.rotate(
                  key: const ValueKey('driver-car'),
                  angle: widget.heading * math.pi / 180,
                  child: Container(
                    decoration: BoxDecoration(
                      color: c.withValues(alpha: 0.25),
                      shape: BoxShape.circle,
                      border: Border.all(color: c, width: 2),
                    ),
                    child: Icon(widget.av ? Icons.smart_toy : Icons.navigation, color: c, size: 22),
                  ),
                ),
              ),
            ]),
            if (basemap != null)
              SimpleAttributionWidget(source: Text(basemap.attribution), backgroundColor: const Color(0xAA0B0F17)),
          ],
        ),
        Positioned(
          left: 10,
          bottom: 10,
          child: Container(
            padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 5),
            decoration: BoxDecoration(color: const Color(0xDD0B0F17), borderRadius: BorderRadius.circular(8)),
            child: const SurgeToggle(),
          ),
        ),
        if (widget.onHorizon != null)
          Positioned(
            left: 10,
            top: 10,
            child: Container(
              padding: const EdgeInsets.all(6),
              decoration: BoxDecoration(color: const Color(0xDD0B0F17), borderRadius: BorderRadius.circular(8)),
              child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
                HorizonSelector(value: widget.horizon ?? 0, onChanged: widget.onHorizon!),
                const SizedBox(height: 4),
                const DemandLegend(),
              ]),
            ),
          ),
        Positioned(
          right: 10,
          top: 10,
          child: FilledButton.tonalIcon(
            key: const ValueKey('driver-recenter'),
            onPressed: () {
              setState(() => _follow = true);
              if (_ready) _map.move(car, _map.camera.zoom);
            },
            icon: Icon(_follow ? Icons.gps_fixed : Icons.gps_not_fixed, size: 16),
            label: Text(_follow ? 'Following' : 'Re-centre'),
          ),
        ),
      ]),
    );
  }
}
