;; withstart -- exports a trapping _start alongside the UDF entry points.
;;
;; _start is a WASI command's main. wazero's default module config CALLS it at
;; instantiation, which for a UDF module would mean running main once per pooled
;; instance -- so the host disables start functions, and this module loads and
;; works only if it does. Initialization that a module genuinely needs belongs in
;; the wasm start section, which instantiation runs regardless.
(module
  (memory (export "memory") 1)
  (func (export "_start") unreachable)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call")
        (param $args i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 11))
    (i32.const 0))
)
