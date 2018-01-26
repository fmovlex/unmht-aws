# unmht-aws

Extracts the ancient MHTML time-table format we use to a user-friendly format (png/plaintext).

This project is mostly an AWS playground turning the humble mht->png CLI to a OCR-backed email based service.

The flow:

```
TimeTables (MHT) ---> HR (Mail) ---> User (FWD)
                                         |
    +------------------------------------+
    |
    v
 AWS SES +----> AWS S3
         |
         +------------> AWS LAMBDA +-----> AWS S3
                                   +-----> AWS REKOGNITION
                                   +-----> AWS SES ------------> User (png + text)
```

The end result:

![mail result](https://i.imgur.com/lljnnFe.png)