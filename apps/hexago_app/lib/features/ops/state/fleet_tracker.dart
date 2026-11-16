import 'package:flutter/foundation.dart';

import '../models/ops_models.dart';

/// Holds the previous and target position of every car so the map can
/// interpolate smoothly between 1 s updates without rebuilding widgets.
class FleetTracker extends ChangeNotifier {
  FleetTracker({
    this.interval = const Duration(milliseconds: 1000),
    this.staleAfter = const Duration(seconds: 15),
    DateTime Function()? clock,
  }) : _clock = clock ?? DateTime.now;

  /// Expected spacing between updates; one interpolation spans this long.
  final Duration interval;

  /// Cars not seen for this long are dropped.
  final Duration staleAfter;
  final DateTime Function() _clock;

  final Map<String, CarTrack> _tracks = {};
  DateTime _updatedAt = DateTime.fromMillisecondsSinceEpoch(0);

  Iterable<CarTrack> get tracks => _tracks.values;
  int get length => _tracks.length;
  bool get isEmpty => _tracks.isEmpty;
  DateTime now() => _clock();

  /// Interpolation progress 0..1 since the last update.
  double progressAt(DateTime now) {
    final ms = now.difference(_updatedAt).inMicroseconds / 1000;
    return (ms / interval.inMilliseconds).clamp(0.0, 1.0);
  }

  void apply(List<DriverPosition> drivers) {
    final now = _clock();
    final t = progressAt(now);
    for (final d in drivers) {
      final track = _tracks[d.id];
      if (track == null) {
        _tracks[d.id] = CarTrack(d, now);
      } else {
        // Start the next leg from wherever the car is currently drawn.
        track.fromLat = track.latAt(t);
        track.fromLng = track.lngAt(t);
        track.fromHeading = track.headingAt(t);
        track.target = d;
        track.seenAt = now;
      }
    }
    _tracks.removeWhere((_, tr) => now.difference(tr.seenAt) > staleAfter);
    _updatedAt = now;
    notifyListeners();
  }
}

class CarTrack {
  CarTrack(this.target, this.seenAt)
      : fromLat = target.lat,
        fromLng = target.lng,
        fromHeading = target.heading;

  DriverPosition target;
  DateTime seenAt;
  double fromLat;
  double fromLng;
  double fromHeading;

  double latAt(double t) => fromLat + (target.lat - fromLat) * t;
  double lngAt(double t) => fromLng + (target.lng - fromLng) * t;

  /// Shortest-arc heading interpolation (350° -> 10° turns through 0°).
  double headingAt(double t) {
    var delta = (target.heading - fromHeading) % 360;
    if (delta > 180) delta -= 360;
    return (fromHeading + delta * t) % 360;
  }
}
