import 'package:flutter/material.dart';

import '../theme/hexago_theme.dart';

/// A small markdown renderer for agent-written reports: `#`/`##`/`###`
/// headings, `|` tables (the `|---|` row is skipped), `-` bullets (two-space
/// indent nests), whole-line `_italic_` notes, inline `**bold**` and
/// `` `code` ``, and plain paragraphs.
class SimpleMarkdown extends StatelessWidget {
  const SimpleMarkdown(this.markdown, {super.key});

  final String markdown;

  static final _inline = RegExp(r'\*\*(.+?)\*\*|`([^`]+)`');

  /// Inline spans of [text]: bold for `**x**`, monospace for `` `x` ``.
  static List<InlineSpan> inline(String text, {TextStyle? base}) {
    final spans = <InlineSpan>[];
    var at = 0;
    for (final m in _inline.allMatches(text)) {
      if (m.start > at) spans.add(TextSpan(text: text.substring(at, m.start), style: base));
      if (m.group(1) != null) {
        spans.add(TextSpan(text: m.group(1), style: (base ?? const TextStyle()).copyWith(fontWeight: FontWeight.w700)));
      } else {
        spans.add(TextSpan(
            text: m.group(2), style: (base ?? const TextStyle()).copyWith(fontFamily: 'monospace', color: HexaColors.cyan)));
      }
      at = m.end;
    }
    if (at < text.length) spans.add(TextSpan(text: text.substring(at), style: base));
    return spans;
  }

  static List<String> _cells(String row) {
    var r = row.trim();
    if (r.startsWith('|')) r = r.substring(1);
    if (r.endsWith('|')) r = r.substring(0, r.length - 1);
    return r.split('|').map((c) => c.trim()).toList();
  }

  static bool _isRule(String row) => RegExp(r'^\|?\s*:?-{3,}').hasMatch(row.trim());

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context).textTheme;
    final children = <Widget>[];
    final lines = markdown.split('\n');
    var i = 0;
    while (i < lines.length) {
      final line = lines[i];
      final t = line.trim();
      if (t.isEmpty) {
        i++;
        continue;
      }
      if (t.startsWith('|')) {
        final rows = <List<String>>[];
        while (i < lines.length && lines[i].trim().startsWith('|')) {
          if (!_isRule(lines[i])) rows.add(_cells(lines[i]));
          i++;
        }
        children.add(_table(rows));
        continue;
      }
      if (t.startsWith('#')) {
        final level = t.indexOf(' ');
        final text = t.substring(level + 1);
        final style = switch (level) {
          1 => theme.titleLarge,
          2 => theme.titleMedium?.copyWith(color: HexaColors.amber),
          _ => theme.titleSmall,
        };
        children.add(Padding(
          padding: EdgeInsets.only(top: level == 1 ? 0 : 12, bottom: 4),
          child: Text(text, style: style),
        ));
      } else if (t.startsWith('- ')) {
        final depth = (line.length - line.trimLeft().length) ~/ 2;
        children.add(Padding(
          padding: EdgeInsets.only(left: 8.0 + depth * 14, top: 2, bottom: 2),
          child: Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
            Text(depth == 0 ? '•  ' : '◦  ', style: const TextStyle(color: HexaColors.textMuted)),
            Expanded(child: Text.rich(TextSpan(children: inline(t.substring(2))), style: const TextStyle(fontSize: 13))),
          ]),
        ));
      } else if (t.length > 1 && t.startsWith('_') && t.endsWith('_')) {
        children.add(Padding(
          padding: const EdgeInsets.symmetric(vertical: 2),
          child: Text(t.substring(1, t.length - 1),
              style: const TextStyle(fontSize: 11, fontStyle: FontStyle.italic, color: HexaColors.textMuted)),
        ));
      } else {
        children.add(Padding(
          padding: const EdgeInsets.symmetric(vertical: 3),
          child: Text.rich(TextSpan(children: inline(t)), style: const TextStyle(fontSize: 13, height: 1.35)),
        ));
      }
      i++;
    }
    return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: children);
  }

  Widget _table(List<List<String>> rows) {
    if (rows.isEmpty) return const SizedBox.shrink();
    final width = rows.map((r) => r.length).reduce((a, b) => a > b ? a : b);
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 4),
      child: Table(
        border: TableBorder.all(color: HexaColors.border, width: 0.5),
        defaultVerticalAlignment: TableCellVerticalAlignment.middle,
        children: [
          for (var r = 0; r < rows.length; r++)
            TableRow(
              decoration: r == 0 ? const BoxDecoration(color: HexaColors.panel) : null,
              children: [
                for (var c = 0; c < width; c++)
                  Padding(
                    padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 4),
                    child: Text.rich(
                      TextSpan(
                        children: inline(c < rows[r].length ? rows[r][c] : '',
                            base: TextStyle(fontSize: 12, fontWeight: r == 0 ? FontWeight.w600 : FontWeight.normal)),
                      ),
                    ),
                  ),
              ],
            ),
        ],
      ),
    );
  }
}
