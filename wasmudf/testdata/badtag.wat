;; badtag -- reports success and leaves a tag that means nothing. The host trusts
;; the status for "was a value written", never for "is it a value".
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call")
        (param $args i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 99))
    (i32.const 0))
)
