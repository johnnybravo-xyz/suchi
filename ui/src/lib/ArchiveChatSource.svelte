<script>
  import { sensitivityLabel, sensDot } from './format.js'
  import Icon from './Icon.svelte'

  let { item, turnID, cited = false, onOpen } = $props()
</script>

<a id={`research-source-${turnID}-${item.number}`} class="source-card" class:cited
   href={`#/doc/${item.source.id}`} onclick={(event) => onOpen?.(event)}
   aria-label={`Open source ${item.number}: ${item.source.title || `Document #${item.source.id}`}`}>
  <span class="source-number">[{item.number}]</span>
  <span class="source-copy">
    <strong>{item.source.title || `Document #${item.source.id}`}</strong>
    <span>{item.source.snippet}</span>
    <small><i class:danger={sensDot(item.source.sensitivity) === 'danger'}></i>{sensitivityLabel(item.source.sensitivity)}</small>
  </span>
  <Icon name="chev" size={13} />
</a>

<style>
  .source-card { width: 100%; display: grid; grid-template-columns: auto minmax(0, 1fr) auto; align-items: start; gap: 9px; padding: 11px; border: 0; border-bottom: 1px solid var(--line); background: transparent; color: inherit; text-align: left; text-decoration: none; cursor: pointer; }
  .source-card:last-child { border-bottom: 0; }
  .source-card:hover, .source-card:focus-visible { background: var(--tint); }
  .source-card.cited { box-shadow: inset 2px 0 var(--accent); }
  .source-number { color: var(--accent); font-family: "Spline Sans Mono", ui-monospace, monospace; font-size: .67rem; font-weight: 700; }
  .source-copy { min-width: 0; display: flex; flex-direction: column; gap: 4px; }
  .source-copy strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: .76rem; }
  .source-copy > span { display: -webkit-box; overflow: hidden; line-clamp: 2; -webkit-line-clamp: 2; -webkit-box-orient: vertical; color: var(--muted); font-size: .7rem; line-height: 1.4; }
  .source-copy small { display: flex; align-items: center; gap: 5px; color: var(--faint); font-size: .63rem; }
  .source-copy i { width: 6px; height: 6px; border-radius: 50%; background: var(--line-strong); }
  .source-copy i.danger { background: var(--danger); }
  .source-card > :global(.ico) { margin-top: 3px; color: var(--faint); }
</style>
