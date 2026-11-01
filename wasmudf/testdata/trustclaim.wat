;; trustclaim -- a module that tries to claim it is trusted, three ways at once:
;; an exported function saying so, an exported global saying so, and a custom
;; section named for the flag.
;;
;; It must load UNTRUSTED. Trust is configuration -- the admin labels the module
;; -- and nothing a module says about itself is consulted, because a capability a
;; module can assert is not a capability, it is a request. This fixture exists so
;; that "the host does not read it" is a test rather than a claim in a comment.
(module
  (@custom "classad_udf_trusted" "\01")
  (memory (export "memory") 1)
  (global (export "classad_udf_trusted_global") i32 (i32.const 1))
  (func (export "classad_udf_trusted") (result i32) (i32.const 1))
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 7))
    (i32.const 0))
)
