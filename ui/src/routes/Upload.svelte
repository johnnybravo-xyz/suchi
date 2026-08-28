<script>
  import UploadBox from '../lib/UploadBox.svelte'
  let { notify, jdCategories = [] } = $props()
</script>

<div class="upload-wrap">
  <UploadBox {notify} {jdCategories} />
</div>
