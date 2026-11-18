import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/rider/models/booking_models.dart';
import 'package:hexago_app/features/rider/widgets/plan_timeline.dart';
import 'package:hexago_app/theme/hexago_theme.dart';

import 'rider_fakes.dart';

Future<void> pumpTimeline(WidgetTester tester, BookingPlan plan,
    {VoidCallback? onConfirm, VoidCallback? onEdit, bool confirming = false, bool confirmed = false}) async {
  tester.view.physicalSize = const Size(900, 1400);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.reset);
  await tester.pumpWidget(MaterialApp(
    theme: buildHexaTheme(),
    home: Scaffold(
      body: SingleChildScrollView(
        child: PlanTimeline(
          plan: plan,
          onConfirm: onConfirm ?? () {},
          onEdit: onEdit ?? () {},
          confirming: confirming,
          confirmed: confirmed,
        ),
      ),
    ),
  ));
}

void main() {
  testWidgets('renders the legs in order with passenger, route, times, tier, price and warnings', (tester) async {
    await pumpTimeline(tester, BookingPlan.fromJson(goldenPlanJson()));

    expect(find.text('Your plan · 2 rides'), findsOneWidget);
    final first = tester.getTopLeft(find.byKey(const Key('leg-1')));
    final second = tester.getTopLeft(find.byKey(const Key('leg-2')));
    expect(first.dy, lessThan(second.dy));

    final leg1 = find.byKey(const Key('leg-1'));
    expect(find.descendant(of: leg1, matching: find.text('Anaya')), findsOneWidget);
    expect(find.descendant(of: leg1, matching: find.text("Anaya's school → Soccer practice")), findsOneWidget);
    expect(find.descendant(of: leg1, matching: find.text('Pickup 3:15 PM')), findsOneWidget);
    expect(find.descendant(of: leg1, matching: find.text('Mini')), findsOneWidget);
    expect(find.descendant(of: leg1, matching: find.text('₹152')), findsOneWidget);
    expect(find.descendant(of: leg1, matching: find.text('1.5x surge')), findsOneWidget);

    final leg2 = find.byKey(const Key('leg-2'));
    expect(find.descendant(of: leg2, matching: find.text('Priya')), findsOneWidget);
    expect(find.descendant(of: leg2, matching: find.text('Office → Kempegowda International Airport (BLR)')),
        findsOneWidget);
    expect(find.descendant(of: leg2, matching: find.text('Arrive by 6:00 PM')), findsOneWidget);
    expect(find.descendant(of: leg2, matching: find.text('Premium sedan')), findsOneWidget);
    expect(find.descendant(of: leg2, matching: find.textContaining('overlap')), findsOneWidget);

    expect(find.text('Total ₹1167'), findsOneWidget);
    expect(find.text('gemini-3.5-flash-lite'), findsOneWidget);
  });

  testWidgets('shows the rule-based tag when the fallback answered', (tester) async {
    await pumpTimeline(tester, BookingPlan.fromJson(goldenPlanJson(fallback: true)));
    expect(find.text('rule-based (forced)'), findsOneWidget);
  });

  testWidgets('Confirm and Edit call back; confirming disables both; confirmed hides them', (tester) async {
    var confirms = 0, edits = 0;
    final plan = BookingPlan.fromJson(goldenPlanJson());
    await pumpTimeline(tester, plan, onConfirm: () => confirms++, onEdit: () => edits++);
    await tester.tap(find.byKey(const Key('plan-confirm')));
    await tester.tap(find.byKey(const Key('plan-edit')));
    expect((confirms, edits), (1, 1));

    await pumpTimeline(tester, plan, onConfirm: () => confirms++, confirming: true);
    expect(find.text('Scheduling…'), findsOneWidget);
    await tester.tap(find.byKey(const Key('plan-confirm')));
    expect(confirms, 1);

    await pumpTimeline(tester, plan, confirmed: true);
    expect(find.byKey(const Key('plan-confirm')), findsNothing);
    expect(find.text('Scheduled'), findsOneWidget);
  });

  testWidgets('plan-level warnings and a single ride', (tester) async {
    final j = goldenPlanJson()
      ..['legs'] = [goldenPlanJson()['legs'][1]]
      ..['warnings'] = ['3:15 PM has already passed today, so the plan is for tomorrow.'];
    await pumpTimeline(tester, BookingPlan.fromJson(j));
    expect(find.text('Your plan · 1 ride'), findsOneWidget);
    expect(find.textContaining('plan is for tomorrow'), findsOneWidget);
  });
}
