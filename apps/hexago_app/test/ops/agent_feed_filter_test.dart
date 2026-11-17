import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';
import 'package:hexago_app/features/ops/widgets/agent_feed.dart';

import 'fakes.dart';

const _ride = '714fcb12-3a4b-4c5d-8e9f-0123456789ab';

Widget _host(List<AgentEvent> events) => MaterialApp(
      home: Scaffold(body: SizedBox(width: 420, height: 800, child: AgentFeed(events: events))),
    );

AgentEvent _match({bool upgraded = false, String id = 'm'}) => sampleEvent(
      id: id,
      kind: 'match',
      rideId: _ride,
      driverId: 'd_042',
      tier: upgraded ? 'sedan' : 'mini',
      requestedTier: 'mini',
      upgraded: upgraded,
      decision: 'matched ride $_ride to d_042 (${upgraded ? 'sedan' : 'mini'}), pickup ETA 2.1 min',
      latencyMs: 0.042,
    );

AgentEvent _noMatch() => sampleEvent(
      id: 'n',
      kind: 'no_match',
      rideId: '99aa0011-3a4b-4c5d-8e9f-0123456789ab',
      requestedTier: 'xl',
      decision: 'no driver available for ride 99aa0011-3a4b-4c5d-8e9f-0123456789ab (xl); request left open',
      reasoning: 'Searched 19 H3 rings out to the 5.0 km pickup radius and found 0 candidates among idle xl drivers.',
    );

AgentEvent _incident() => sampleEvent(
      id: 'i',
      agent: 'safety_sentinel',
      kind: 'incident',
      rideId: _ride,
      driverId: 'd_007',
      decision: 'flagged d_007 on ride $_ride: 450 m off planned route',
      usedFallback: true,
    );

void main() {
  testWidgets('decision shows the short ride id, full id only when expanded', (tester) async {
    await tester.pumpWidget(_host([_match()]));
    expect(find.text('matched ride #714fcb to d_042 (mini), pickup ETA 2.1 min'), findsOneWidget);
    expect(find.textContaining(_ride), findsNothing);
    expect(find.text('#714fcb'), findsOneWidget); // ride tag
    expect(find.text('d_042'), findsOneWidget); // driver tag
    expect(find.text('42 µs'), findsOneWidget);

    await tester.tap(find.byType(AgentEventTile));
    await tester.pump(const Duration(milliseconds: 300));
    final ids = find.byKey(const ValueKey('event-ids'));
    expect(ids, findsOneWidget);
    expect(find.descendant(of: ids, matching: find.textContaining(_ride)), findsOneWidget);
    expect(find.descendant(of: ids, matching: find.textContaining('d_042')), findsOneWidget);
  });

  testWidgets('tier and UPGRADED tags', (tester) async {
    await tester.pumpWidget(_host([_match(upgraded: true)]));
    expect(find.byKey(const ValueKey('tier-tag')), findsOneWidget);
    expect(find.text('SEDAN'), findsOneWidget);
    expect(find.byKey(const ValueKey('upgraded-tag')), findsOneWidget);
    expect(find.text('UPGRADED'), findsOneWidget);

    await tester.pumpWidget(_host([_match()]));
    expect(find.text('MINI'), findsOneWidget);
    expect(find.byKey(const ValueKey('upgraded-tag')), findsNothing);
  });

  testWidgets('filter chips narrow the feed', (tester) async {
    await tester.pumpWidget(_host([_match(), _noMatch(), _incident(), sampleEvent(id: 'x', agent: 'fleet_simulator', decision: 'scenario rush_hour started')]));
    Finder chip(String label) => find.byKey(ValueKey('feed-filter-$label'));
    int tiles() => tester.widgetList(find.byType(AgentEventTile)).length;

    for (final l in ['All', 'Matches', 'Unmatched', 'Incidents']) {
      expect(chip(l), findsOneWidget, reason: l);
    }
    expect(tiles(), 4);

    await tester.tap(chip('Matches'));
    await tester.pump();
    expect(tiles(), 1);
    expect(find.textContaining('matched ride #714fcb'), findsOneWidget);

    await tester.tap(chip('Unmatched'));
    await tester.pump();
    expect(tiles(), 1);
    expect(find.textContaining('no driver available for ride #99aa00'), findsOneWidget);

    await tester.tap(chip('Incidents'));
    await tester.pump();
    expect(tiles(), 1);
    expect(find.text('Safety Sentinel'), findsOneWidget);

    await tester.tap(chip('All'));
    await tester.pump();
    expect(tiles(), 4);
  });

  testWidgets('empty filter result says so', (tester) async {
    await tester.pumpWidget(_host([_match()]));
    await tester.tap(find.byKey(const ValueKey('feed-filter-Incidents')));
    await tester.pump();
    expect(find.text('No incidents yet'), findsOneWidget);
  });

  group('AgentEvent.category', () {
    test('uses the kind field', () {
      expect(_match().category, FeedCategory.match);
      expect(_noMatch().category, FeedCategory.unmatched);
      expect(_incident().category, FeedCategory.incident);
    });
    test('falls back to the text for older servers', () {
      expect(sampleEvent(decision: 'matched ride abc to d_1').category, FeedCategory.match);
      expect(sampleEvent(decision: 'no driver available for ride abc').category, FeedCategory.unmatched);
      expect(sampleEvent(agent: 'safety_sentinel', decision: 'flagged d_1').category, FeedCategory.incident);
      expect(sampleEvent(agent: 'fleet_simulator', decision: 'scenario started').category, FeedCategory.other);
    });
  });

  test('parses the structured fields', () {
    final e = AgentEvent.fromJson({
      'type': 'agent_event', 'agent': 'dispatch_broker', 'decision': 'd', 'reasoning': 'r', 'confidence': 0.8,
      'latency_ms': 0.0424, 'used_fallback': false, 'ts': '2026-11-16T10:30:00Z',
      'kind': 'match', 'ride_id': _ride, 'driver_id': 'd_7', 'tier': 'sedan', 'requested_tier': 'mini', 'upgraded': true,
    });
    expect(e.latencyMs, 0.0424);
    expect(e.kind, 'match');
    expect(e.rideId, _ride);
    expect(e.driverId, 'd_7');
    expect(e.tier, 'sedan');
    expect(e.requestedTier, 'mini');
    expect(e.upgraded, isTrue);
    expect(e.toJson()['ride_id'], _ride);
    final old = AgentEvent.fromJson({'agent': 'a'});
    expect(old.kind, '');
    expect(old.upgraded, isFalse);
  });
}
