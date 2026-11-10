'use strict';
(function (root) {
  function splitLines(text) {
    return text.match(/[^\n]*\n|[^\n]+$/g) || [];
  }
  function range(hunk, side) {
    return [hunk[side + 'Start'] ?? hunk.BaseStart, hunk[side + 'End'] ?? hunk.BaseEnd];
  }
  function makeLineRows(text, hunks, side) {
    const source = splitLines(text), anchors = new Map();
    const addAnchor = (index, hunk, change, label) => {
      index = Math.max(0, Math.min(source.length, index));
      const rows = anchors.get(index) || [];
      if (!rows.some(row => row.hunks.includes(hunk.id))) rows.push({type:'anchor', index, text:'', hunks:[hunk.id], kind:change?.Kind || 'context', label});
      anchors.set(index, rows);
    };
    for (const hunk of hunks) {
      const changes = hunk[side + 'Changes'] || [];
      for (const change of changes) {
        if (change.Start !== change.End) continue;
        const sourceName = change.Side === 'remote' ? '云端' : change.Side === 'local' ? '本地' : side === 'Remote' ? '云端' : '本地';
        const label = side === 'Base' ? `${sourceName}在此插入内容` : change.Kind === 'delete' ? `${sourceName}在此删除原版 ${change.BaseEnd - change.BaseStart} 行` : '此处没有对应文本';
        addAnchor(change.Start, hunk, change, label);
      }
      const [start,end] = range(hunk,side);
      if (start === end) addAnchor(start,hunk,null,'此处没有对应文本');
    }
    const rows = [];
    for (let i = 0; i <= source.length; i++) {
      rows.push(...(anchors.get(i) || []));
      if (i === source.length) break;
      const containing = hunks.filter(h => { const [start,end] = range(h,side); return i >= start && i < end; });
      const changes = containing.flatMap(h => h[side + 'Changes'] || []).filter(c => i >= c.Start && i < c.End);
      rows.push({type:'line', index:i, number:i+1, text:source[i], hunks:containing.map(h=>h.id), changed:changes.length > 0, kind:changes[0]?.Kind || ''});
    }
    return rows;
  }
  function mergeChoices(base, hunks, choices) {
    const source = splitLines(base); let cursor = 0, text = '';
    for (const hunk of hunks) {
      if (hunk.BaseStart < cursor || hunk.BaseEnd < hunk.BaseStart || hunk.BaseEnd > source.length) throw new Error('差异范围已变化，请重新获取差异。');
      const choice = choices.get(hunk.id) || 'Local';
      if (!['Base','Local','Remote'].includes(choice)) throw new Error('未知差异选项。');
      text += source.slice(cursor,hunk.BaseStart).join('') + hunk[choice]; cursor = hunk.BaseEnd;
    }
    return text + source.slice(cursor).join('');
  }
  function snippet(text, maxLines = 2, maxChars = 140) {
    if (!text) return '（此处无内容）';
    const all = splitLines(text), selected = all.slice(0,maxLines).join('').replace(/\r?\n$/,'');
    const chars = Array.from(selected), clipped = chars.length > maxChars;
    return chars.slice(0,maxChars).join('') + (clipped || all.length > maxLines ? '…' : '');
  }
  const helpers = {splitLines,makeLineRows,mergeChoices,snippet};
  if (typeof module !== 'undefined' && module.exports) module.exports = helpers;
  else root.DriveBridgeDiff = helpers;
})(globalThis);
