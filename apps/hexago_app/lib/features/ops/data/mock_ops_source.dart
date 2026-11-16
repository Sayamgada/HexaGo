import 'dart:async';
import 'dart:math' as math;

import '../models/ops_models.dart';
import 'ops_source.dart';

/// Local simulation of a 300-car Bengaluru fleet plus agent swarm activity.
///
/// Positions advance once per second. Movement is time-compressed
/// ([speedup]) so cars visibly move at city zoom levels.
class MockOpsSource implements OpsSource {
  MockOpsSource({
    this.fleetSize = 300,
    this.centerLat = 12.9716,
    this.centerLng = 77.5946,
    this.speedup = 15,
    this.tick = const Duration(seconds: 1),
    int? seed,
  }) : _rng = math.Random(seed);

  final int fleetSize;
  final double centerLat;
  final double centerLng;
  final double speedup;
  final Duration tick;
  final math.Random _rng;

  // Rough bounding box of the Bengaluru urban area around the centre.
  static const double _latSpan = 0.11;
  static const double _lngSpan = 0.13;

  // M. Chinnaswamy Stadium, used by the concert-exit scenario.
  static const double _venueLat = 12.9788;
  static const double _venueLng = 77.5996;

  final _drivers = StreamController<List<DriverPosition>>.broadcast();
  final _events = StreamController<AgentEvent>.broadcast();
  final _metrics = StreamController<DispatchMetrics>.broadcast();
  final _status = StreamController<OpsConnectionStatus>.broadcast();

  final List<_SimCar> _cars = [];
  Timer? _tickTimer;
  Timer? _eventTimer;
  DispatchMetrics? _latestMetrics;
  bool _started = false;
  bool _disposed = false;

  int _matchesTotal = 0;
  double _openRequests = 40;
  Scenario? _scenario;
  int _scenarioTicks = 0;

  /// Current fleet snapshot (exposed for tests).
  List<DriverPosition> get snapshot => _cars.map((c) => c.toPosition()).toList();

  @override
  String get modeLabel => 'MOCK';
  @override
  Stream<List<DriverPosition>> get drivers => _drivers.stream;
  @override
  Stream<AgentEvent> get agentEvents => _events.stream;
  @override
  Stream<DispatchMetrics> get metrics => _metrics.stream;
  @override
  Stream<OpsConnectionStatus> get status => _status.stream;
  @override
  OpsConnectionStatus get latestStatus => OpsConnectionStatus.live;
  @override
  DispatchMetrics? get latestMetrics => _latestMetrics;

  @override
  void start() {
    if (_started || _disposed) return;
    _started = true;
    _seedFleet();
    // Plausible "today" baseline: ~0.35 matches per second since midnight.
    final now = DateTime.now();
    final secondsToday = now.hour * 3600 + now.minute * 60 + now.second;
    _matchesTotal = 1200 + (secondsToday * 0.35).round();
    Timer.run(_step);
    _tickTimer = Timer.periodic(tick, (_) => _step());
    _eventTimer = Timer.periodic(tick * 0.4, (_) {
      if (_rng.nextDouble() < 0.45 + (_scenario != null ? 0.3 : 0)) {
        _emitEvent(_randomEvent());
      }
    });
  }

  void _seedFleet() {
    _cars.clear();
    for (var i = 0; i < fleetSize; i++) {
      final r = _rng.nextDouble();
      final status = r < 0.35
          ? DriverStatus.idle
          : r < 0.5
              ? DriverStatus.toPickup
              : r < 0.95
                  ? DriverStatus.onTrip
                  : DriverStatus.offline;
      final tierRoll = _rng.nextDouble();
      final tier = tierRoll < 0.12
          ? 'av'
          : tierRoll < 0.3
              ? 'premium'
              : tierRoll < 0.45
                  ? 'xl'
                  : 'economy';
      // Cluster around the centre with a gaussian-ish spread.
      final lat = centerLat + _gauss() * _latSpan * 0.4;
      final lng = centerLng + _gauss() * _lngSpan * 0.4;
      _cars.add(_SimCar(
        id: 'drv_${(i + 1).toString().padLeft(3, '0')}',
        lat: lat,
        lng: lng,
        heading: (_rng.nextInt(8) * 45).toDouble(),
        status: status,
        tier: tier,
        battery: (tier == 'av' || _rng.nextDouble() < 0.3) ? 35 + _rng.nextDouble() * 65 : null,
      ));
    }
  }

  double _gauss() {
    // Box-Muller, clamped to +-2.5 sigma.
    final u1 = math.max(_rng.nextDouble(), 1e-9);
    final u2 = _rng.nextDouble();
    final z = math.sqrt(-2 * math.log(u1)) * math.cos(2 * math.pi * u2);
    return z.clamp(-2.5, 2.5);
  }

  void _step() {
    if (_disposed) return;
    final rain = _scenario == Scenario.rainStorm;
    for (final car in _cars) {
      _advance(car, rain: rain);
    }
    _matchesTotal += 1 + _rng.nextInt(_scenario == null ? 2 : 4);
    _updateOpenRequests();
    if (_scenario != null && --_scenarioTicks <= 0) {
      if (_scenario == Scenario.safetyIncident) {
        for (final c in _cars.where((c) => c.status == DriverStatus.incident)) {
          c.status = DriverStatus.onTrip;
        }
      }
      _scenario = null;
    }
    _drivers.add(snapshot);
    _emitMetrics(_computeMetrics());
  }

  void _advance(_SimCar car, {required bool rain}) {
    if (car.status == DriverStatus.offline) {
      car.speedKmh = 0;
      if (_rng.nextDouble() < 0.01) car.status = DriverStatus.idle;
      return;
    }
    if (car.status == DriverStatus.incident) {
      car.speedKmh = 0;
      return;
    }

    // Status transitions.
    final roll = _rng.nextDouble();
    final rush = _scenario == Scenario.rushHour || _scenario == Scenario.concertExit;
    switch (car.status) {
      case DriverStatus.idle:
        if (roll < (rush ? 0.08 : 0.025)) car.status = DriverStatus.toPickup;
        if (roll > 0.998) car.status = DriverStatus.offline;
      case DriverStatus.toPickup:
        if (roll < 0.04) car.status = DriverStatus.onTrip;
      case DriverStatus.onTrip:
        if (roll < 0.012) car.status = DriverStatus.idle;
      default:
        break;
    }

    // Speed target by status, slower in rain.
    final base = switch (car.status) {
      DriverStatus.idle => 8.0,
      DriverStatus.toPickup => 28.0,
      DriverStatus.onTrip => 32.0,
      _ => 0.0,
    };
    final target = base * (rain ? 0.55 : 1) * (0.7 + _rng.nextDouble() * 0.6);
    car.speedKmh += (target - car.speedKmh) * 0.4;

    // Concert exit: idle cars converge on the venue.
    if (_scenario == Scenario.concertExit &&
        (car.status == DriverStatus.idle || car.status == DriverStatus.toPickup) &&
        _rng.nextDouble() < 0.3) {
      car.heading = _bearing(car.lat, car.lng, _venueLat, _venueLng);
    } else if (_rng.nextDouble() < 0.08) {
      // Grid-like turns (Bengaluru roads are not, but it reads well).
      car.heading = (car.heading + (_rng.nextBool() ? 90 : -90) + (_rng.nextDouble() < 0.3 ? 45 : 0)) % 360;
    }

    // Steer back toward the centre if drifting out of the box.
    if ((car.lat - centerLat).abs() > _latSpan || (car.lng - centerLng).abs() > _lngSpan) {
      car.heading = _bearing(car.lat, car.lng, centerLat, centerLng);
    }

    final km = car.speedKmh / 3600 * tick.inMilliseconds / 1000 * speedup;
    final rad = car.heading * math.pi / 180;
    car.lat += km / 111.0 * math.cos(rad);
    car.lng += km / (111.0 * math.cos(car.lat * math.pi / 180)) * math.sin(rad);
    if (car.battery != null) {
      car.battery = math.max(5, car.battery! - 0.002 * car.speedKmh / 10);
    }
  }

  double _bearing(double lat1, double lng1, double lat2, double lng2) {
    final dy = lat2 - lat1;
    final dx = (lng2 - lng1) * math.cos(lat1 * math.pi / 180);
    return (math.atan2(dx, dy) * 180 / math.pi + 360) % 360;
  }

  void _updateOpenRequests() {
    final target = switch (_scenario) {
      Scenario.rushHour => 160.0,
      Scenario.concertExit => 220.0,
      Scenario.rainStorm => 120.0,
      _ => 35.0,
    };
    _openRequests += (target - _openRequests) * 0.15 + (_rng.nextDouble() - 0.5) * 6;
    _openRequests = math.max(0, _openRequests);
  }

  DispatchMetrics _computeMetrics() {
    var idle = 0, active = 0, onTrip = 0;
    for (final c in _cars) {
      if (c.status == DriverStatus.offline) continue;
      active++;
      if (c.status == DriverStatus.idle) idle++;
      if (c.status == DriverStatus.onTrip || c.status == DriverStatus.incident) onTrip++;
    }
    final load = _openRequests / 100;
    final p50 = 14 + load * 10 + _rng.nextDouble() * 6;
    final p95 = p50 * (1.6 + _rng.nextDouble() * 0.3);
    final p99 = p95 * (1.15 + _rng.nextDouble() * 0.25);
    return DispatchMetrics(
      p50Ms: p50,
      p95Ms: p95,
      p99Ms: p99,
      matchesTotal: _matchesTotal,
      activeDrivers: active,
      idleDrivers: idle,
      openRequests: _openRequests.round(),
      ridesInProgress: onTrip,
    );
  }

  void _emitMetrics(DispatchMetrics m) {
    _latestMetrics = m;
    _metrics.add(m);
  }

  void _emitEvent(AgentEvent e) {
    if (!_disposed) _events.add(e);
  }

  AgentEvent _event(String agent, String decision, String reasoning, {double? confidence, bool? fallback}) {
    final usedFallback = fallback ?? _rng.nextDouble() < 0.07;
    return AgentEvent(
      agent: agent,
      decision: decision,
      reasoning: usedFallback
          ? '$reasoning LLM call exceeded the latency budget, so the deterministic rule engine decided.'
          : reasoning,
      confidence: usedFallback ? 1.0 : (confidence ?? 0.72 + _rng.nextDouble() * 0.27),
      latencyMs: usedFallback ? 3 + _rng.nextDouble() * 9 : 60 + _rng.nextDouble() * 800,
      usedFallback: usedFallback,
      ts: DateTime.now().toUtc(),
    );
  }

  String _pick(List<String> xs) => xs[_rng.nextInt(xs.length)];

  String get _area => _pick(const [
        'Koramangala', 'Indiranagar', 'Whitefield', 'HSR Layout', 'MG Road', 'Jayanagar',
        'Malleshwaram', 'Hebbal', 'Electronic City', 'Marathahalli', 'BTM Layout', 'Yeshwanthpur',
      ]);

  String get _driver => _cars.isEmpty ? 'drv_001' : _cars[_rng.nextInt(_cars.length)].id;

  AgentEvent _randomEvent() {
    final n = _rng.nextInt(11);
    final area = _area;
    switch (n) {
      case 0:
        return _event('Booking Intent', 'Parsed "drop me near the metro by 6" → $area Metro, ETA-bound pickup',
            'Resolved the vague landmark against the station gazetteer; picked the nearest exit to rider GPS.');
      case 1:
        final eta = 2 + _rng.nextInt(6);
        return _event('Dispatch Broker', 'Matched rider in $area to $_driver (ETA $eta min)',
            'Searched H3 k-ring 2 around the pickup cell; ranked 7 idle candidates by ETA, acceptance score and heading alignment.');
      case 2:
        return _event('Pool Batching', 'Pooled 2 riders $area → MG Road (+9% ETA)',
            'Trajectories overlap 82% within the 90 s batch window; added detour stays under the 15% threshold.');
      case 3:
        final m = (1.0 + _rng.nextDouble() * 0.8).toStringAsFixed(1);
        return _event('Surge Equilibrium', 'Set $area multiplier to ${m}x',
            'Demand/supply ratio in 3 adjacent hex cells rose to ${(1.2 + _rng.nextDouble()).toStringAsFixed(2)}; elasticity model predicts ${m}x clears the queue in ~6 min.');
      case 4:
        return _event('Demand Forecast', 'Pre-positioning 6 drivers toward $area (+30 min spike)',
            'Office egress pattern for this weekday/hour bucket plus live app-open signals forecast a 38% demand jump.');
      case 5:
        return _event('Safety Sentinel', 'Cleared route deviation for $_driver',
            'Vehicle left planned path by 220 m for a known road closure; within tolerance and rider acknowledged.');
      case 6:
        return _event('Compliance Vision', 'Approved inspection photos for $_driver',
            'Plate OCR matches registration; tyre tread and lighting checks pass; insurance valid for 214 days.');
      case 7:
        return _event('Legal & Jurisdiction', 'Enforced 12 h shift cap for $_driver',
            'Karnataka aggregator guidelines limit continuous duty; driver logged 11 h 52 m, offers paused.');
      case 8:
        final amt = 80 + _rng.nextInt(400);
        return _event('Settlement', 'Settled trip payout ₹$amt to $_driver',
            'Distance and duration fare minus platform take-rate; no disputes or adjustments flagged.');
      case 9:
        return _event('Support & Dispute', 'Refunded ₹${40 + _rng.nextInt(120)} for route dispute in $area',
            'Telemetry shows a 2.1 km detour not requested by rider; matched the credit matrix tier B.');
      default:
        return _event('Ops Supervisor', 'Swarm healthy: 11/11 agents within guardrails',
            'Reasoning-drift score 0.04, fallback rate ${(_rng.nextDouble() * 8).toStringAsFixed(1)}%, no circuit breakers open.');
    }
  }

  @override
  Future<void> triggerScenario(Scenario scenario) async {
    if (_disposed) return;
    _scenario = scenario;
    _scenarioTicks = 45;
    final area = _area;
    switch (scenario) {
      case Scenario.rushHour:
        _emitEvent(_event('Demand Forecast', 'Rush hour detected: demand +140% across 18 hex cells',
            'Request rate crossed the 95th percentile for this hour bucket within 4 minutes.', fallback: false));
        _emitEvent(_event('Surge Equilibrium', 'Raised surge to 1.6x in CBD cells',
            'Elastic pricing to pull idle supply from the outer ring; capped by jurisdiction rules.', fallback: false));
        _emitEvent(_event('Pool Batching', 'Widened pool window to 120 s',
            'High trajectory overlap on outbound corridors makes pooling 31% more efficient.', fallback: false));
      case Scenario.rainStorm:
        _emitEvent(_event('Safety Sentinel', 'Rain mode: lowered speed alert threshold to 40 km/h',
            'IMD feed reports heavy rain; road friction model lowered safe speeds citywide.', fallback: false));
        _emitEvent(_event('Surge Equilibrium', 'Surge 1.4x with rider-protection cap',
            'Demand up 60% while supply drops; fairness guardrail limits multiplier during weather events.', fallback: false));
        _emitEvent(_event('Dispatch Broker', 'Extended match radius to H3 k-ring 4',
            'Slower traffic raises ETAs; widening search keeps p95 pickup ETA under 9 min.', fallback: false));
      case Scenario.concertExit:
        _emitEvent(_event('Demand Forecast', 'Concert exit at Chinnaswamy Stadium: ~4,000 riders in 20 min',
            'Event calendar plus geofenced app-opens show a sharp egress spike.', fallback: false));
        _emitEvent(_event('Dispatch Broker', 'Staging 40 drivers at 3 pickup zones near Cubbon Road',
            'Distributing pickups avoids gridlock at the main gate.', fallback: false));
        _emitEvent(_event('Pool Batching', 'Pooling riders heading to $area',
            'Many riders share outbound corridors; 3-seat pools cut queue time by half.', fallback: false));
      case Scenario.safetyIncident:
        final onTrip = _cars.where((c) => c.status == DriverStatus.onTrip).toList();
        final car = onTrip.isEmpty ? (_cars.isEmpty ? null : _cars.first) : onTrip[_rng.nextInt(onTrip.length)];
        car?.status = DriverStatus.incident;
        final id = car?.id ?? 'drv_001';
        _emitEvent(_event('Safety Sentinel', 'Sudden stop + 640 m route deviation on $id: SOS check-in sent',
            'Deceleration spike of 0.8 g followed by stationary 90 s off the planned path in a low-traffic zone.',
            confidence: 0.93, fallback: false));
        _emitEvent(_event('Ops Supervisor', 'Escalated $id incident to emergency protocol',
            'No rider response within 30 s; notified emergency contact and shared live location.', fallback: false));
        _emitEvent(_event('Support & Dispute', 'Opened safety case for $id, trip fare held',
            'Fare settlement paused until the safety review closes.', fallback: false));
    }
  }

  @override
  Future<void> dispose() async {
    _disposed = true;
    _tickTimer?.cancel();
    _eventTimer?.cancel();
    await Future.wait([
      _drivers.close(),
      _events.close(),
      _metrics.close(),
      _status.close(),
    ]);
  }
}

class _SimCar {
  _SimCar({
    required this.id,
    required this.lat,
    required this.lng,
    required this.heading,
    required this.status,
    required this.tier,
    this.battery,
  });

  final String id;
  double lat;
  double lng;
  double heading;
  double speedKmh = 0;
  DriverStatus status;
  final String tier;
  double? battery;

  DriverPosition toPosition() => DriverPosition(
        id: id,
        lat: lat,
        lng: lng,
        heading: heading,
        speedKmh: speedKmh,
        status: status,
        tier: tier,
        battery: battery == null ? null : (battery! * 10).roundToDouble() / 10,
      );
}
