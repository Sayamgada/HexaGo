import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_map/flutter_map.dart';
import 'package:latlong2/latlong.dart' show LatLng;

import '../../../theme/hexago_theme.dart';
import '../../ops/data/basemap.dart';
import '../models/booking_models.dart';

String legStatusLabel(ScheduledLeg l, DateTime now) {
  switch (l.status) {
    case 'scheduled':
      final at = l.dispatchAt;
      if (at == null) return 'Waiting for the previous ride';
      final s = at.difference(now).inSeconds;
      return s > 0 ? 'Dispatching in ${s}s' : 'Dispatching…';
    case 'dispatched':
      return switch (l.rideStatus) {
        'requested' => 'Finding a driver…',
        'matched' => 'Driver ${l.driverId ?? ''} on the way',
        'in_progress' => 'On the trip',
        _ => 'Dispatched',
      };
    case 'completed':
      return 'Completed';
    case 'cancelled':
      return 'Cancelled${l.note.isEmpty ? '' : ': ${l.note}'}';
    default:
      return 'Could not dispatch${l.note.isEmpty ? '' : ': ${l.note}'}';
  }
}

Color legStatusColor(ScheduledLeg l) => switch (l.status) {
      'dispatched' => l.rideStatus == 'in_progress' ? HexaColors.onTrip : HexaColors.toPickup,
      'completed' => HexaColors.good,
      'cancelled' || 'failed' => HexaColors.bad,
      _ => HexaColors.textMuted,
    };

/// The plan's scheduled legs; the live one is shown on the map.
class ScheduledLegsPanel extends StatelessWidget {
  const ScheduledLegsPanel({super.key, required this.legs, required this.now, this.live, this.showMap = true});
  final List<ScheduledLeg> legs;
  final DateTime now;
  final Widget? live;
  final bool showMap;

  @override
  Widget build(BuildContext context) {
    return Card(
      key: const Key('scheduled-panel'),
      color: HexaColors.panel,
      child: Padding(
        padding: const EdgeInsets.all(14),
        child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
          const Text('Scheduled rides', style: TextStyle(fontWeight: FontWeight.bold, fontSize: 16)),
          const SizedBox(height: 8),
          for (final l in legs)
            ListTile(
              key: Key('scheduled-${l.seq}'),
              dense: true,
              contentPadding: EdgeInsets.zero,
              leading: CircleAvatar(radius: 12, backgroundColor: legStatusColor(l), child: Text('${l.seq}',
                  style: const TextStyle(fontSize: 11, color: Colors.black))),
              title: Text('${l.passengerName}: ${l.pickupName.split(' - ').first} → ${l.dropoffName.split(' - ').first}'),
              subtitle: Text(legStatusLabel(l, now), style: TextStyle(color: legStatusColor(l))),
              trailing: Text(l.tier),
            ),
          if (showMap && live != null) ...[const SizedBox(height: 8), live!],
        ]),
      ),
    );
  }
}

/// Live map for one dispatched leg: pickup, drop-off and the driver from `ride:<id>`.
class LiveRideMap extends StatefulWidget {
  const LiveRideMap({super.key, required this.leg, required this.updates});
  final ScheduledLeg leg;
  final Stream<RideUpdate> updates;

  @override
  State<LiveRideMap> createState() => _LiveRideMapState();
}

class _LiveRideMapState extends State<LiveRideMap> {
  StreamSubscription<RideUpdate>? _sub;
  RideUpdate? _last;

  @override
  void initState() {
    super.initState();
    _sub = widget.updates.listen((u) => setState(() => _last = u), onError: (_) {});
  }

  @override
  void dispose() {
    _sub?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final l = widget.leg;
    final pickup = LatLng(l.pickupLat, l.pickupLng);
    final drop = LatLng(l.dropoffLat, l.dropoffLng);
    final u = _last;
    final driver = u?.driverLat != null ? LatLng(u!.driverLat!, u.driverLng!) : null;
    const basemap = Basemap.cartoDark;
    return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
      Text(
        u == null
            ? 'Live: ride ${l.rideId}'
            : 'Live: ${u.status.replaceAll('_', ' ')}${u.etaSec > 0 ? ' · ETA ${(u.etaSec / 60).ceil()} min' : ''}',
        key: const Key('live-ride-status'),
        style: const TextStyle(color: HexaColors.cyan),
      ),
      const SizedBox(height: 6),
      ClipRRect(
        borderRadius: BorderRadius.circular(10),
        child: SizedBox(
          height: 260,
          child: FlutterMap(
            options: MapOptions(
              initialCameraFit: CameraFit.bounds(bounds: LatLngBounds(pickup, drop), padding: const EdgeInsets.all(40)),
            ),
            children: [
              TileLayer(urlTemplate: basemap.urlTemplate, subdomains: basemap.subdomains,
                  userAgentPackageName: 'com.hexago.app'),
              PolylineLayer(polylines: [Polyline(points: [pickup, drop], color: HexaColors.amber, strokeWidth: 3)]),
              MarkerLayer(markers: [
                Marker(point: pickup, child: const Icon(Icons.trip_origin, color: HexaColors.good)),
                Marker(point: drop, child: const Icon(Icons.flag, color: HexaColors.amber)),
                if (driver != null) Marker(point: driver, child: const Icon(Icons.local_taxi, color: HexaColors.cyan)),
              ]),
            ],
          ),
        ),
      ),
    ]);
  }
}
