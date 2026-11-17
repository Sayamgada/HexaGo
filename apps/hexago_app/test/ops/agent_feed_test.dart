import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';
import 'package:hexago_app/features/ops/widgets/agent_feed.dart';

import 'fakes.dart';

Widget _host(List<AgentEvent> events, {int? total}) => MaterialApp(
      home: Scaffold(
        body: SizedBox(width: 420, height: 700, child: AgentFeed(events: events, totalSeen: total)),
      ),
    );

void main() {
  testWidgets('empty feed shows a waiting message', (tester) async {
    await tester.pumpWidget(_host(const []));
    expect(find.text('Live Agent Feed'), findsOneWidget);
    expect(find.text('Waiting for agent decisions…'), findsOneWidget);
  });

  testWidgets('renders agent name, decision, confidence and latency', (tester) async {
    await tester.pumpWidget(_host([sampleEvent()], total: 1234));

    expect(find.text('Dispatch Broker'), findsOneWidget);
    expect(find.text('Matched rider in Koramangala to drv_042 (ETA 3 min)'), findsOneWidget);
    expect(find.text('91% conf'), findsOneWidget);
    expect(find.text('142.00 ms'), findsOneWidget);
    expect(find.text('1,234 events'), findsOneWidget);
    expect(find.byKey(const ValueKey('fallback-tag')), findsNothing);
  });

  testWidgets('shows the FALLBACK tag only on fallback decisions', (tester) async {
    await tester.pumpWidget(_host([
      sampleEvent(id: 'a', agent: 'Safety Sentinel', usedFallback: true, decision: 'Rule engine decided'),
      sampleEvent(id: 'b', agent: 'Settlement', decision: 'LLM decided'),
    ]));
    expect(find.byKey(const ValueKey('fallback-tag')), findsOneWidget);
    expect(find.text('FALLBACK'), findsOneWidget);
    final tagTile = find.ancestor(of: find.text('FALLBACK'), matching: find.byType(AgentEventTile));
    expect(find.descendant(of: tagTile, matching: find.text('Safety Sentinel')), findsOneWidget);
  });

  testWidgets('tapping an event expands and collapses its reasoning', (tester) async {
    await tester.pumpWidget(_host([sampleEvent()]));
    final reasoning = find.text('Searched H3 k-ring 2; ranked 7 idle candidates by ETA.');
    expect(reasoning, findsNothing);

    await tester.tap(find.byType(AgentEventTile));
    await tester.pump(const Duration(milliseconds: 300));
    expect(reasoning, findsOneWidget);
    expect(find.byKey(const ValueKey('event-reasoning')), findsOneWidget);

    await tester.tap(find.byType(AgentEventTile));
    await tester.pump(const Duration(milliseconds: 300));
    expect(reasoning, findsNothing);
  });

  testWidgets('lists events in the order given (newest first)', (tester) async {
    await tester.pumpWidget(_host([
      sampleEvent(id: '1', decision: 'newest'),
      sampleEvent(id: '2', decision: 'older'),
    ]));
    final newest = tester.getTopLeft(find.text('newest')).dy;
    final older = tester.getTopLeft(find.text('older')).dy;
    expect(newest, lessThan(older));
  });

  test('agent style resolves roster names in any casing', () {
    expect(AgentStyle.keyFor('dispatch_broker'), 'dispatchbroker');
    expect(AgentStyle.keyFor('Dispatch Broker'), 'dispatchbroker');
    expect(AgentStyle.keyFor('DispatchBrokerAgent'), 'dispatchbroker');
    expect(AgentStyle.keyFor('Legal & Jurisdiction'), 'legaljurisdiction');
    expect(AgentStyle.keyFor('support_and_dispute'), 'supportdispute');
    expect(AgentStyle.keyFor('demand_forecast'), 'demandforecast');
    expect(displayAgentName('pool_batching'), 'Pool Batching');
    expect(displayAgentName('Ops Supervisor'), 'Ops Supervisor');
  });
}
