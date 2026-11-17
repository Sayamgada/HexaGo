import 'dart:async';
import 'dart:math' as math;

import 'package:flutter/material.dart';
import 'package:flutter/scheduler.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:latlong2/latlong.dart' show LatLng;

import '../../../config/app_config.dart';
import '../../../theme/hexago_theme.dart';
import '../../../widgets/hexagon.dart';
import '../data/basemap.dart';
import '../models/ops_models.dart';
import '../state/fleet_tracker.dart';
import '../state/ops_providers.dart';

Color statusColor(DriverStatus s) => switch (s) {
      DriverStatus.idle => HexaColors.idle,
      DriverStatus.toPickup => HexaColors.toPickup,
      DriverStatus.onTrip => HexaColors.onTrip,
      DriverStatus.offline => HexaColors.offline,
      DriverStatus.incident => HexaColors.incident,
      DriverStatus.unknown => HexaColors.unknown,
    };

/// City map: keyless CARTO dark tiles (OSM if CARTO serves no imagery) and
/// one painted layer for all cars. The first fleet snapshot fits the camera
/// to every vehicle; after that the operator controls the view.
class FleetMap extends ConsumerStatefulWidget {
  const FleetMap({super.key});

  @override
  ConsumerState<FleetMap> createState() => _FleetMapState();
}

class _FleetMapState extends ConsumerState<FleetMap> {
  final MapController _map = MapController();
  FleetTracker? _tracker;
  bool _ready = false;
  bool _fitted = false;

  /// Tiles are held back until the camera has settled (fitted to the fleet,
  /// or [_fitTimeout] without a fleet). Tiles requested for the initial
  /// camera and replaced mid-flight by the fit were left unpainted by
  /// flutter_map on the live site until the next manual pan/zoom.
  bool _settled = false;
  Timer? _settleTimer;
  static const _fitTimeout = Duration(seconds: 4);

  void _attach(FleetTracker tracker) {
    if (identical(tracker, _tracker)) return;
    _tracker?.removeListener(_maybeFit);
    _tracker = tracker..addListener(_maybeFit);
  }

  void _maybeFit() {
    final t = _tracker;
    if (_fitted || !_ready || t == null || t.isEmpty) return;
    final bounds = fleetBounds(t.tracks.map((c) => LatLng(c.target.lat, c.target.lng)));
    if (bounds == null) return;
    _fitted = true;
    _map.fitCamera(CameraFit.bounds(bounds: bounds, padding: const EdgeInsets.fromLTRB(40, 70, 40, 50), maxZoom: 14));
    _settle();
  }

  void _settle() {
    _settleTimer?.cancel();
    if (_settled || !mounted) return;
    setState(() => _settled = true);
  }

  @override
  void dispose() {
    _settleTimer?.cancel();
    _tracker?.removeListener(_maybeFit);
    _map.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    _attach(ref.watch(fleetTrackerProvider));
    final basemap = ref.watch(showMapTilesProvider) && _settled ? ref.watch(basemapProvider).value : null;
    return ClipRRect(
      borderRadius: BorderRadius.circular(12),
      child: Stack(
        children: [
          const Positioned.fill(
            child: ColoredBox(
              color: Color(0xFF0B0F17),
              child: HexGridBackground(color: Color(0x0FFFFFFF), radius: 22),
            ),
          ),
          FlutterMap(
            mapController: _map,
            options: MapOptions(
              // Bengaluru until the first fleet snapshot arrives.
              initialCenter: const LatLng(AppConfig.cityLat, AppConfig.cityLng),
              initialZoom: 11,
              minZoom: 8,
              maxZoom: 18,
              backgroundColor: Colors.transparent,
              interactionOptions: const InteractionOptions(flags: InteractiveFlag.all & ~InteractiveFlag.rotate),
              onMapReady: () {
                _ready = true;
                _settleTimer = Timer(_fitTimeout, _settle);
                _maybeFit();
              },
            ),
            children: [
              if (basemap != null)
                TileLayer(
                  key: ValueKey('basemap-${basemap.name}'),
                  urlTemplate: basemap.urlTemplate,
                  fallbackUrl: basemap.fallbackUrl,
                  subdomains: basemap.subdomains,
                  retinaMode: basemap.subdomains.isNotEmpty && RetinaMode.isHighDensity(context),
                  tileBuilder: basemap.darkFilter ? darkModeTileBuilder : null,
                  userAgentPackageName: 'com.hexago.hexago_app',
                  maxZoom: 19,
                ),
              const FleetLayer(),
              if (basemap != null)
                SimpleAttributionWidget(
                  key: const ValueKey('basemap-attribution'),
                  source: Text(basemap.attribution),
                  backgroundColor: const Color(0xAA0B0F17),
                ),
            ],
          ),
          const Positioned(left: 10, bottom: 10, child: _Legend()),
        ],
      ),
    );
  }
}

/// Draws every car in a single [CustomPaint]. Positions are interpolated
/// on each frame by a ticker that only runs while a transition is in flight,
/// so 300+ cars cost one repaint per frame and zero widget rebuilds.
class FleetLayer extends ConsumerStatefulWidget {
  const FleetLayer({super.key});

  @override
  ConsumerState<FleetLayer> createState() => _FleetLayerState();
}

class _FleetLayerState extends ConsumerState<FleetLayer> with SingleTickerProviderStateMixin {
  late final Ticker _ticker = createTicker(_onTick);
  final ValueNotifier<int> _frame = ValueNotifier(0);
  FleetTracker? _tracker;

  void _onTracker() {
    if (!_ticker.isActive) _ticker.start();
  }

  void _onTick(Duration _) {
    _frame.value++;
    final t = _tracker;
    if (t == null || t.progressAt(t.now()) >= 1) _ticker.stop();
  }

  void _attach(FleetTracker tracker) {
    if (identical(tracker, _tracker)) return;
    _tracker?.removeListener(_onTracker);
    _tracker = tracker..addListener(_onTracker);
  }

  @override
  void dispose() {
    _tracker?.removeListener(_onTracker);
    _ticker.dispose();
    _frame.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final tracker = ref.watch(fleetTrackerProvider);
    _attach(tracker);
    final camera = MapCamera.of(context);
    return IgnorePointer(
      child: SizedBox.expand(
        child: RepaintBoundary(
          child: CustomPaint(
            key: const ValueKey('fleet-layer'),
            painter: FleetPainter(tracker: tracker, camera: camera, repaint: _frame),
          ),
        ),
      ),
    );
  }
}

class FleetPainter extends CustomPainter {
  FleetPainter({required this.tracker, required this.camera, required Listenable repaint})
      : super(repaint: Listenable.merge([repaint, tracker]));

  final FleetTracker tracker;
  final MapCamera camera;

  static final Path _body = _carPath();
  static final Map<Color, Paint> _fills = {};
  static final Paint _avOutline = Paint()
    ..style = PaintingStyle.stroke
    ..strokeWidth = 1.4
    ..color = Colors.white;

  /// Arrow-ish car silhouette pointing north, centred on the origin.
  static Path _carPath() {
    return Path()
      ..moveTo(0, -7)
      ..quadraticBezierTo(3.6, -5.5, 3.6, -2)
      ..lineTo(3.6, 5)
      ..quadraticBezierTo(3.6, 6.5, 2, 6.5)
      ..lineTo(-2, 6.5)
      ..quadraticBezierTo(-3.6, 6.5, -3.6, 5)
      ..lineTo(-3.6, -2)
      ..quadraticBezierTo(-3.6, -5.5, 0, -7)
      ..close();
  }

  @override
  void paint(Canvas canvas, Size size) {
    if (tracker.isEmpty) return;
    final t = tracker.progressAt(tracker.now());
    final scale = ((camera.zoom - 9) / 4).clamp(0.55, 1.5);
    final bounds = (Offset.zero & size).inflate(12);
    final rad = math.pi / 180;

    // Paint incidents last so they sit on top.
    final incidents = <(Offset, CarTrack)>[];
    for (final car in tracker.tracks) {
      final p = camera.latLngToScreenOffset(LatLng(car.latAt(t), car.lngAt(t)));
      if (!bounds.contains(p)) continue;
      if (car.target.status == DriverStatus.incident) {
        incidents.add((p, car));
        continue;
      }
      _drawCar(canvas, p, car, t, scale, rad);
    }
    for (final (p, car) in incidents) {
      canvas.drawCircle(p, 14 * scale, Paint()..color = HexaColors.incident.withValues(alpha: 0.25));
      _drawCar(canvas, p, car, t, scale * 1.3, rad);
    }
  }

  void _drawCar(Canvas canvas, Offset p, CarTrack car, double t, double scale, double rad) {
    final color = statusColor(car.target.status);
    final fill = _fills.putIfAbsent(color, () => Paint()..color = color);
    canvas.save();
    canvas.translate(p.dx, p.dy);
    canvas.rotate(car.headingAt(t) * rad);
    canvas.scale(scale);
    canvas.drawPath(_body, fill);
    if (car.target.isAutonomous) {
      canvas.drawPath(_body, _avOutline);
      canvas.drawCircle(Offset.zero, 1.4, _avOutline);
    }
    canvas.restore();
  }

  @override
  bool shouldRepaint(FleetPainter old) => old.camera != camera || old.tracker != tracker;
}

class _Legend extends StatelessWidget {
  const _Legend();

  @override
  Widget build(BuildContext context) {
    Widget item(Color c, String label, {bool av = false}) => Padding(
          padding: const EdgeInsets.only(right: 10),
          child: Row(mainAxisSize: MainAxisSize.min, children: [
            Container(
              width: 9,
              height: 9,
              decoration: BoxDecoration(
                color: c,
                shape: BoxShape.circle,
                border: av ? Border.all(color: Colors.white, width: 1.5) : null,
              ),
            ),
            const SizedBox(width: 4),
            Text(label, style: const TextStyle(fontSize: 10.5)),
          ]),
        );
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
      decoration: BoxDecoration(
        color: const Color(0xCC0B0F17),
        borderRadius: BorderRadius.circular(8),
        border: Border.all(color: HexaColors.border),
      ),
      child: Wrap(children: [
        item(HexaColors.idle, 'Idle'),
        item(HexaColors.toPickup, 'To pickup'),
        item(HexaColors.onTrip, 'On trip'),
        item(HexaColors.incident, 'Incident'),
        item(HexaColors.offline, 'Offline'),
        item(Colors.transparent, 'Autonomous (AV)', av: true),
      ]),
    );
  }
}
