;; stale -- writes a result for an integer argument and, for anything else,
;; reports success while writing nothing.
;;
;; That second case is the one that matters. Instances are reused across records,
;; so the result slot of a call that writes nothing holds the PREVIOUS record's
;; answer: without the host poisoning the slot first, a string argument here comes
;; back as the integer the last call returned, with a success status and nothing
;; anywhere reporting a problem. It is the silent-wrong-answer shape, written down
;; as a module so a test can insist the host does not have it.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call")
        (param $args i32) (param $argc i32) (param $res i32) (result i32)
    (if (i32.eq (i32.load (local.get $args)) (i32.const 3))
      (then (memory.copy (local.get $res) (local.get $args) (i32.const 16))))
    (i32.const 0))
)
