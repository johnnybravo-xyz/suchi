<script>
  // Hand-drawn 24×24 stroke icons in the brand's line weight.
  // No icon dependency; each is a few path commands.
  let { name, size = 17 } = $props()
  const paths = {
    docs: 'M5 4h9l5 5v11H5zM14 4v5h5M8.5 13h7M8.5 16.5h5',
    inbox: 'M4 5h16v14H4zM4 13h5l1.5 2.5h3L15 13h5',
    search: 'M10.5 4a6.5 6.5 0 1 0 0 13 6.5 6.5 0 0 0 0-13zM15.5 15.5 20 20',
    tasks: 'M5 4h14v16H5zM8.5 9l1.8 1.8L13.5 7.5M8.5 15h7',
    zap: 'M13 3 5 13.5h5.5L11 21l8-10.5h-5.5z',
    upload: 'M12 16V5M7.5 9 12 4.5 16.5 9M5 19h14',
    settings: 'M4 8h8M16 8h4M4 16h4M12 16h8M14 5.5v5M10 13.5v5',
    user: 'M12 4a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM4.5 20a7.5 7.5 0 0 1 15 0',
    sun: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5 5l1.4 1.4M17.6 17.6 19 19M5 19l1.4-1.4M17.6 6.4 19 5',
    moon: 'M20 13.5A8 8 0 0 1 10.5 4 8 8 0 1 0 20 13.5z',
    out: 'M14 4h5v16h-5M10 8l-4 4 4 4M6 12h10',
    x: 'M6 6l12 12M18 6 6 18',
    check: 'M5 12.5 10 17.5 19 7',
    eye: 'M3 12s3.5-6 9-6 9 6 9 6-3.5 6-9 6-9-6-9-6zM12 9.5a2.5 2.5 0 1 0 0 5 2.5 2.5 0 0 0 0-5z',
    link: 'M9.5 14.5 14.5 9.5M8 11 5.5 13.5a3.5 3.5 0 0 0 5 5L13 16M16 13l2.5-2.5a3.5 3.5 0 0 0-5-5L11 8',
    trash: 'M5 7h14M9.5 7V4.5h5V7M7 7l1 13h8l1-13M10.5 11v5M13.5 11v5',
    plus: 'M12 5v14M5 12h14',
    left: 'M14.5 5.5 8 12l6.5 6.5',
    chev: 'M9.5 5.5 16 12l-6.5 6.5',
    gauge: 'M12 4a8 8 0 0 0-8 8h16a8 8 0 0 0-8-8zM12 12l4-3.5M4 12v4h16v-4',
    bell: 'M6 16v-5a6 6 0 0 1 12 0v5l1.5 2.5h-15zM10 18.5a2 2 0 0 0 4 0',
    download: 'M12 4v11M7.5 11 12 15.5 16.5 11M5 19.5h14',
    lock: 'M7 11V8a5 5 0 0 1 10 0v3M5.5 11h13V20h-13zM12 14.5v2.5',
    shield: 'M12 3 5 6v5c0 4.5 3 8 7 10 4-2 7-5.5 7-10V6z',
    mail: 'M4 6h16v12H4zM4.5 7l7.5 6 7.5-6',
    menu: 'M4 6h16M4 12h16M4 18h16',
  }
</script>

<svg class="ico" width={size} height={size} viewBox="0 0 24 24" fill="none"
     stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"
     aria-hidden="true">
  <path d={paths[name] || ''} />
</svg>
