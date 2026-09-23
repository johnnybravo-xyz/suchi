<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script>
  import UploadBox from '../lib/UploadBox.svelte'
  let { notify, jdCategories = [] } = $props()
</script>

<div class="upload-wrap">
  <UploadBox {notify} {jdCategories} />
</div>
