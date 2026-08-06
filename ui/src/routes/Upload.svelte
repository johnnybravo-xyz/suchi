<script>
  import UploadBox from '../lib/UploadBox.svelte'
  let { notify } = $props()
</script>

<div class="upload-wrap">
  <UploadBox {notify} />
</div>
